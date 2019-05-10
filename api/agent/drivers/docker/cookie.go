package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	units "github.com/docker/go-units"
	"github.com/fnproject/fn/api/agent/drivers"
	"github.com/fnproject/fn/api/common"
	"github.com/fnproject/fn/api/models"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

const (
	FnUserId  = 1000
	FnGroupId = 1000
)

var (
	ErrImageWithVolume = models.NewAPIError(http.StatusBadRequest, errors.New("image has Volume definition"))
	// FnDockerUser is used as the runtime user/group when running docker containers.
	// This is not configurable at the moment, because some fdks require that user/group to be present in the container.
	FnDockerUser = fmt.Sprintf("%v:%v", FnUserId, FnGroupId)
)

// A cookie identifies a unique request to run a task.
type cookie struct {
	// namespace id used from prefork pool if applicable
	poolId string
	// network name from docker networks if applicable
	netId string

	// docker container create options created by Driver.CreateCookie
	opts     *container.Config
	hostOpts *container.HostConfig
	// task associated with this cookie
	task drivers.ContainerTask
	// pointer to docker driver
	drv *DockerDriver

	imgReg string

	// contains inspected image if ValidateImage() is called
	image *CachedImage

	// contains created container if CreateContainer() is called
	containerCreated bool
}

func (c *cookie) configureImage(log logrus.FieldLogger) {
	c.imgReg, _, _ = drivers.ParseImage(c.task.Image())
}

func (c *cookie) configureLabels(log logrus.FieldLogger) {
	if c.drv.conf.ContainerLabelTag == "" {
		return
	}

	if c.opts.Labels == nil {
		c.opts.Labels = make(map[string]string)
	}

	c.opts.Labels[FnAgentClassifierLabel] = c.drv.conf.ContainerLabelTag
	c.opts.Labels[FnAgentInstanceLabel] = c.drv.instanceId
}

func (c *cookie) configureLogger(log logrus.FieldLogger) {

	conf := c.task.LoggerConfig()
	if conf.URL == "" {
		c.hostOpts.LogConfig = container.LogConfig{
			Type: "none",
		}
		return
	}

	c.hostOpts.LogConfig = container.LogConfig{
		Type: "syslog",
		Config: map[string]string{
			"syslog-address":  conf.URL,
			"syslog-facility": "user",
			"syslog-format":   "rfc5424",
		},
	}

	tags := make([]string, 0, len(conf.Tags))
	for _, pair := range conf.Tags {
		tags = append(tags, fmt.Sprintf("%s=%s", pair.Name, pair.Value))
	}
	if len(tags) > 0 {
		c.hostOpts.LogConfig.Config["tag"] = strings.Join(tags, ",")
	}
}

func (c *cookie) configureMem(log logrus.FieldLogger) {
	if c.task.Memory() == 0 {
		return
	}

	mem := int64(c.task.Memory())

	c.hostOpts.Memory = mem
	c.hostOpts.MemorySwap = mem // disables swap TODO(reed): this doesn't seem to actually work but swappiness disables anyway?
	c.hostOpts.KernelMemory = mem
	var zero int64
	c.hostOpts.MemorySwappiness = &zero
}

func (c *cookie) configureFsSize(log logrus.FieldLogger) {
	if c.task.FsSize() == 0 {
		return
	}

	// If defined, impose file system size limit. In MB units.
	if c.hostOpts.StorageOpt == nil {
		c.hostOpts.StorageOpt = make(map[string]string)
	}

	opt := fmt.Sprintf("%vM", c.task.FsSize())
	log.WithFields(logrus.Fields{"size": opt, "call_id": c.task.Id()}).Debug("setting storage option")
	c.hostOpts.StorageOpt["size"] = opt
}

func (c *cookie) configurePIDs(log logrus.FieldLogger) {
	pids := c.task.PIDs()
	if pids == 0 {
		return
	}

	pids64 := int64(pids)
	log.WithFields(logrus.Fields{"pids": pids64, "call_id": c.task.Id()}).Debug("setting PIDs")
	c.hostOpts.PidsLimit = &pids64
}

func (c *cookie) configureULimits(log logrus.FieldLogger) {
	c.configureULimit("nofile", c.task.OpenFiles(), log)
	c.configureULimit("memlock", c.task.LockedMemory(), log)
	c.configureULimit("sigpending", c.task.PendingSignals(), log)
	c.configureULimit("msgqueue", c.task.MessageQueue(), log)
}

func (c *cookie) configureULimit(name string, value *uint64, log logrus.FieldLogger) {
	if value == nil {
		return
	}

	log = log.WithFields(logrus.Fields{"call_id": c.task.Id(), "ulimitName": name, "ulimitValue": *value})

	value64 := int64(*value)
	if value64 < 0 {
		log.Warnf("ulimit value too big (ulimit ignored): %s", name)
		return
	}

	log.Debugf("setting ulimit %s", name)
	c.hostOpts.Ulimits = append(c.hostOpts.Ulimits, &units.Ulimit{Name: name, Soft: value64, Hard: value64})
}

func (c *cookie) configureTmpFs(log logrus.FieldLogger) {
	// if RO Root is NOT enabled and TmpFsSize does not have any limit, then we do not need
	// any tmpfs in the container since function can freely write whereever it wants.
	if c.task.TmpFsSize() == 0 && !c.drv.conf.EnableReadOnlyRootFs {
		return
	}

	if c.hostOpts.Tmpfs == nil {
		c.hostOpts.Tmpfs = make(map[string]string)
	}

	var tmpFsOption string
	if c.task.TmpFsSize() != 0 {
		if c.drv.conf.MaxTmpFsInodes != 0 {
			tmpFsOption = fmt.Sprintf("size=%dm,nr_inodes=%d", c.task.TmpFsSize(), c.drv.conf.MaxTmpFsInodes)
		} else {
			tmpFsOption = fmt.Sprintf("size=%dm", c.task.TmpFsSize())
		}
	}

	log.WithFields(logrus.Fields{"target": "/tmp", "options": tmpFsOption, "call_id": c.task.Id()}).Debug("setting tmpfs")
	c.hostOpts.Tmpfs["/tmp"] = tmpFsOption
}

func (c *cookie) configureIOFS(log logrus.FieldLogger) {
	path := c.task.UDSDockerPath()
	if path == "" {
		// TODO this should be required soon-ish
		return
	}

	bind := fmt.Sprintf("%s:%s", path, c.task.UDSDockerDest())
	c.hostOpts.Binds = append(c.hostOpts.Binds, bind)
	log.WithFields(logrus.Fields{"bind": bind, "call_id": c.task.Id()}).Debug("setting bind")
}

func (c *cookie) configureVolumes(log logrus.FieldLogger) {
	if len(c.task.Volumes()) == 0 {
		return
	}

	if c.opts.Volumes == nil {
		c.opts.Volumes = map[string]struct{}{}
	}

	for _, mapping := range c.task.Volumes() {
		hostDir := mapping[0]
		containerDir := mapping[1]
		c.opts.Volumes[containerDir] = struct{}{}
		mapn := fmt.Sprintf("%s:%s", hostDir, containerDir)
		c.hostOpts.Binds = append(c.hostOpts.Binds, mapn)
		log.WithFields(logrus.Fields{"volumes": mapn, "call_id": c.task.Id()}).Debug("setting volumes")
	}
}

func (c *cookie) configureCPU(log logrus.FieldLogger) {
	// Translate milli cpus into CPUQuota & CPUPeriod (see Linux cGroups CFS cgroup v1 documentation)
	// eg: task.CPUQuota() of 8000 means CPUQuota of 8 * 100000 usecs in 100000 usec period,
	// which is approx 8 CPUS in CFS world.
	// Also see docker run options --cpu-quota and --cpu-period
	if c.task.CPUs() == 0 {
		return
	}

	quota := int64(c.task.CPUs() * 100)
	period := int64(100000)

	log.WithFields(logrus.Fields{"quota": quota, "period": period, "call_id": c.task.Id()}).Debug("setting CPU")
	c.hostOpts.CPUQuota = quota
	c.hostOpts.CPUPeriod = period
}

func (c *cookie) configureWorkDir(log logrus.FieldLogger) {
	wd := c.task.WorkDir()
	if wd == "" {
		return
	}

	log.WithFields(logrus.Fields{"wd": wd, "call_id": c.task.Id()}).Debug("setting work dir")
	c.opts.WorkingDir = wd
}

func (c *cookie) configureNetwork(log logrus.FieldLogger) {
	if c.hostOpts.NetworkMode != "" {
		return
	}

	if c.task.DisableNet() {
		c.hostOpts.NetworkMode = "none"
		return
	}

	// If pool is enabled, we try to pick network from pool
	if c.drv.pool != nil {
		id, err := c.drv.pool.AllocPoolId()
		if id != "" {
			// We are able to fetch a container from pool. Now, use its
			// network, ipc and pid namespaces.
			c.hostOpts.NetworkMode = container.NetworkMode(fmt.Sprintf("container:%s", id))
			//c.hostOpts.IpcMode = linker
			//c.hostOpts.PidMode = linker
			c.poolId = id
			return
		}
		if err != nil {
			log.WithError(err).Error("Could not fetch pre fork pool container")
		}
	}

	// if pool is not enabled or fails, then pick from defined networks if any
	id := c.drv.network.AllocNetwork()
	if id != "" {
		c.hostOpts.NetworkMode = container.NetworkMode(id)
		c.netId = id
	}
}

func (c *cookie) configureHostname(log logrus.FieldLogger) {
	// hostname and container NetworkMode is not compatible.
	if c.hostOpts.NetworkMode != "" {
		return
	}

	log.WithFields(logrus.Fields{"hostname": c.drv.hostname, "call_id": c.task.Id()}).Debug("setting hostname")
	c.opts.Hostname = c.drv.hostname
}

func (c *cookie) configureCmd(log logrus.FieldLogger) {
	if c.task.Command() == "" {
		return
	}

	// NOTE: this is hyper-sensitive and may not be correct like this even, but it passes old tests
	cmd := strings.Fields(c.task.Command())
	log.WithFields(logrus.Fields{"call_id": c.task.Id(), "cmd": cmd, "len": len(cmd)}).Debug("docker command")
	c.opts.Cmd = cmd
}

func (c *cookie) configureEnv(log logrus.FieldLogger) {
	if len(c.task.EnvVars()) == 0 {
		return
	}

	if c.opts.Env == nil {
		c.opts.Env = make([]string, 0, len(c.task.EnvVars()))
	}

	for name, val := range c.task.EnvVars() {
		c.opts.Env = append(c.opts.Env, name+"="+val)
	}
}

func (c *cookie) configureSecurity(log logrus.FieldLogger) {
	if c.drv.conf.DisableUnprivilegedContainers {
		return
	}
	c.opts.User = FnDockerUser
	c.hostOpts.CapDrop = []string{"all"}
	c.hostOpts.SecurityOpt = []string{"no-new-privileges:true"}
	log.WithFields(logrus.Fields{"user": c.opts.User, "CapDrop": c.hostOpts.CapDrop, "SecurityOpt": c.hostOpts.SecurityOpt, "call_id": c.task.Id()}).Debug("setting security")
}

// implements Cookie
func (c *cookie) Close(ctx context.Context) error {
	var err error
	if c.containerCreated {
		err = c.drv.docker.RemoveContainer(docker.RemoveContainerOptions{
			ID: c.task.Id(), Force: true, RemoveVolumes: true, Context: ctx})
		if err != nil {
			common.Logger(ctx).WithError(err).WithFields(logrus.Fields{"call_id": c.task.Id()}).Error("error removing container")
		}
	}

	if c.poolId != "" && c.drv.pool != nil {
		c.drv.pool.FreePoolId(c.poolId)
	}
	if c.netId != "" {
		c.drv.network.FreeNetwork(c.netId)
	}

	if c.image != nil && c.drv.imgCache != nil {
		c.drv.imgCache.MarkFree(c.image)
	}
	return err
}

// implements Cookie
func (c *cookie) Run(ctx context.Context) (drivers.WaitResult, error) {
	return c.drv.run(ctx, c.task)
}

// implements Cookie
func (c *cookie) ContainerOptions() interface{} {
	return c.opts
}

// implements Cookie
func (c *cookie) Freeze(ctx context.Context) error {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "Freeze"})
	log.WithFields(logrus.Fields{"call_id": c.task.Id()}).Debug("docker pause")

	err := c.drv.docker.PauseContainer(c.task.Id(), ctx)
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{"call_id": c.task.Id()}).Error("error pausing container")
	}
	return err
}

// implements Cookie
func (c *cookie) Unfreeze(ctx context.Context) error {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "Unfreeze"})
	log.WithFields(logrus.Fields{"call_id": c.task.Id()}).Debug("docker unpause")

	err := c.drv.docker.UnpauseContainer(c.task.Id(), ctx)
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{"call_id": c.task.Id()}).Error("error unpausing container")
	}
	return err
}

func (c *cookie) authImage(ctx context.Context) (types.AuthConfig, error) {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "AuthImage"})
	log.WithFields(logrus.Fields{"call_id": c.task.Id()}).Debug("docker auth image")

	// ask for docker creds before looking for image, as the tasker may need to
	// validate creds even if the image is downloaded.
	config := findRegistryConfig(c.imgReg, c.drv.auths)

	if task, ok := c.task.(Auther); ok {
		_, span := trace.StartSpan(ctx, "docker_auth")
		authConfig, err := task.DockerAuth(ctx, c.task.Image())
		span.End()
		if err != nil {
			return config, err
		}
		if authConfig != nil {
			config = *authConfig
		}
	}

	return config, nil
}

// implements Cookie
func (c *cookie) ValidateImage(ctx context.Context) (bool, error) {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "ValidateImage"})
	log.WithFields(logrus.Fields{"call_id": c.task.Id(), "image": c.task.Image()}).Debug("docker inspect image")

	if c.image != nil {
		return false, nil
	}

	// see if we already have it
	// TODO this should use the image cache instead of making a docker call
	img, _, err := c.drv.docker.ImageInspectWithRaw(ctx, c.task.Image())
	if err == docker.ErrNoSuchImage {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	// check image doesn't have Volumes
	if !c.drv.conf.ImageEnableVolume && img.Config != nil && len(img.Config.Volumes) > 0 {
		err = ErrImageWithVolume
	}

	c.image = &CachedImage{
		ID:       img.ID,
		ParentID: img.Parent,
		RepoTags: img.RepoTags,
		Size:     uint64(img.Size),
	}

	if c.drv.imgCache != nil {
		if err == ErrImageWithVolume {
			c.drv.imgCache.Update(c.image)
		} else {
			c.drv.imgCache.MarkBusy(c.image)
		}
	}
	return false, err
}

// implements Cookie
func (c *cookie) PullImage(ctx context.Context) error {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "PullImage"})
	if c.image != nil {
		return nil
	}

	cfg, err := c.authImage(ctx)
	if err != nil {
		return err
	}

	log = common.Logger(ctx).WithFields(logrus.Fields{"registry": cfg.ServerAddress, "username": cfg.Username})
	log.WithFields(logrus.Fields{"call_id": c.task.Id(), "image": c.task.Image()}).Debug("docker pull")
	ctx = common.WithLogger(ctx, log)

	errC := c.drv.imgPuller.PullImage(ctx, cfg, c.task.Image())
	return <-errC
}

// implements Cookie
func (c *cookie) CreateContainer(ctx context.Context) error {
	ctx, log := common.LoggerWithFields(ctx, logrus.Fields{"stack": "CreateContainer"})
	log.WithFields(logrus.Fields{"call_id": c.task.Id(), "image": c.task.Image()}).Debug("docker create container")

	if c.image == nil {
		log.Fatal("invalid usage: image not validated")
	}
	if c.containerCreated {
		return nil
	}

	var err error

	opts := c.opts
	hostOpts := c.hostOpts

	_, err = c.drv.docker.ContainerCreate(ctx, opts, hostOpts, nil, c.task.Id())
	c.containerCreated = true

	// IMPORTANT: The return code 503 here is controversial. Here we treat disk pressure as a temporary
	// service too busy event that will likely to correct itself. Here with 503 we allow this request
	// to land on another (or back to same runner) which will likely to succeed. We have received
	// docker.ErrNoSuchImage because just after PullImage(), image cleaner (or manual intervention)
	// must have removed this image.
	if err == docker.ErrNoSuchImage {
		log.WithError(err).Error("Cannot CreateContainer image likely removed")
		return models.ErrCallTimeoutServerBusy
	}

	if err != nil {
		log.WithError(err).Error("Could not create container")
		return err
	}

	return nil
}

var _ drivers.Cookie = &cookie{}
