// Copyright 2021 CloudJ Company Limited. All rights reserved.

package runner

import (
	"context"
	"path/filepath"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"

	"cloudiac/common"
	"cloudiac/configs"
	"cloudiac/utils"
)

// Task Executor
type Executor struct {
	Image      string
	Env        []string
	Timeout    int
	PrivateKey string

	TerraformVersion string
	Commands         []string
	HostWorkdir      string // 宿主机目录
	Workdir          string // 容器目录
	// for container
	//ContainerInstance *Container
}

// Container Info
type Container struct {
	Context context.Context
	ID      string
	RunID   string
}

func DockerClient() (*client.Client, error) {
	return dockerClient()
}

func dockerClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create docker client")
	}
	return cli, nil
}

func (exec *Executor) Start() (string, error) {
	logger := logger.WithField("taskId", filepath.Base(exec.HostWorkdir))
	cli, err := dockerClient()
	if err != nil {
		logger.Error(err)
		return "", err
	}

	conf := configs.Get()
	mountConfigs := []mount.Mount{
		{
			Type:   mount.TypeBind,
			Source: exec.HostWorkdir,
			Target: ContainerWorkspace,
		},
		{
			Type:   mount.TypeBind,
			Source: conf.Runner.AbsPluginCachePath(),
			Target: ContainerPluginCachePath,
		},
		{
			Type:   mount.TypeBind,
			Source: "/var/run/docker.sock",
			Target: "/var/run/docker.sock",
		},
	}

	// assets_path 配置为空则表示直接使用 worker 容器中打包的 assets。
	// 在 runner 容器化部署时运行 runner 的宿主机(docker host)并没有 assets 目录，
	// 如果配置了 assets 路径，进行 bind mount 时会因为源目录不存在而报错。
	if conf.Runner.AssetsPath != "" {
		mountConfigs = append(mountConfigs, mount.Mount{
			Type:     mount.TypeBind,
			Source:   conf.Runner.AbsAssetsPath(),
			Target:   ContainerAssetsDir,
			ReadOnly: true,
		})
		mountConfigs = append(mountConfigs, mount.Mount{
			// providers 需要挂载到指定目录才能被 terraform 查找到，所以单独做一次挂载
			Type:     mount.TypeBind,
			Source:   conf.Runner.ProviderPath(),
			Target:   ContainerPluginPath,
			ReadOnly: true,
		})
	}

	// 内置 tf 版本列表中无该版本，我们挂载缓存目录到容器，下载后会保存到宿主机，下次可以直接使用。
	// 注意，该方案有个问题：客户无法自定义镜像预先安装需要的 terraform 版本，
	// 因为判断版本不在 TerraformVersions 列表中就会挂载目录，客户自定义镜像安装的版本会被覆盖
	//（考虑把版本列表写到配置文件？）
	if !utils.StrInArray(exec.TerraformVersion, common.TerraformVersions...) {
		mountConfigs = append(mountConfigs, mount.Mount{
			Type:   mount.TypeBind,
			Source: conf.Runner.AbsTfenvVersionsCachePath(),
			Target: "/root/.tfenv/versions",
		})
	}

	c, err := cli.ContainerCreate(
		context.Background(),
		&container.Config{
			Image:        exec.Image,
			WorkingDir:   exec.Workdir,
			Cmd:          exec.Commands,
			Env:          exec.Env,
			OpenStdin:    true,
			Tty:          true,
			AttachStdin:  false,
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			AutoRemove: false,
			Mounts:     mountConfigs,
		},
		nil,
		nil,
		"")
	if err != nil {
		logger.Errorf("create container err: %v", err)
		return "", err
	}

	cid := utils.ShortContainerId(c.ID)
	logger.Infof("container id: %s", cid)
	err = cli.ContainerStart(context.Background(), c.ID, types.ContainerStartOptions{})
	return cid, err
}

func (Executor) RunCommand(cid string, command []string) (execId string, err error) {
	cli, err := dockerClient()
	if err != nil {
		logger.Warn(err)
		return "", err
	}

	resp, err := cli.ContainerExecCreate(context.Background(), cid, types.ExecConfig{
		Detach: false,
		Cmd:    command,
	})
	if err != nil {
		err = errors.Wrap(err, "container exec create")
		logger.Warn(err)
		return "", err
	}

	err = cli.ContainerExecStart(context.Background(), resp.ID, types.ExecStartCheck{})
	if err != nil {
		err = errors.Wrap(err, "container exec start")
		logger.Warn(err)
		return "", err
	}

	// cli.ContainerExecCreate()
	// resp, err := cli.ContainerAttach(context.Background(), cid, types.ContainerAttachOptions{
	// 	Stream: true,
	// 	Stdin:  true,
	// 	Stdout: true,
	// 	Stderr: true,
	// })
	// if err != nil {
	// 	return errors.Wrap(err, "container attach")
	// }
	// defer resp.Close()

	// logger.Debug("send command: %s", command)
	// if _, err := resp.Conn.Write(append([]byte(command), '\n')); err != nil {
	// 	return errors.Wrap(err, "send command")
	// }
	return resp.ID, nil
}

func (Executor) GetExecInfo(execId string) (execInfo types.ContainerExecInspect, err error) {
	cli, err := dockerClient()
	if err != nil {
		return execInfo, err
	}
	execInfo, err = cli.ContainerExecInspect(context.Background(), execId)
	if err != nil {
		return execInfo, errors.Wrap(err, "container exec attach")
	}
	return execInfo, nil
}

func (Executor) WaitCommand(ctx context.Context, execId string) (execInfo types.ContainerExecInspect, err error) {
	cli, err := dockerClient()
	if err != nil {
		return execInfo, err
	}

	for {
		inspect, err := cli.ContainerExecInspect(ctx, execId)
		if err != nil {
			return execInfo, errors.Wrap(err, "container exec attach")
		}
		if !inspect.Running {
			return execInfo, nil
		}
	}
}
