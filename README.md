# API Proxy

一个轻量级的 API 代理服务，支持多 Key 轮询访问、自动重试和并发限流等功能。

## 主要功能

- **多 Key 管理与自动切换**：支持配置多个上游 API 的认证 Key。请求失败或 Key 额度耗尽时，能够自动标记失效并重试其他可用的 Key。
- **限流与并发控制**：内置速率限制 (Rate Limiter) 和请求体限制 (Max Body Size) 保护，防止请求过载，确保服务的稳定运行。
- **灵活的路由与配置**：支持通过 YAML 配置文件自定义监听端口、上游服务器地址、自定义 Auth Header 等参数。
- **轻量易部署**：极简的部署方式，无外部数据库依赖，编译后为单文件。

## 如何运行服务

在启动服务前，你需要提供一个配置文件。你可以通过以下命令生成并查看配置文件的参考模板：

```bash
./api-proxy -print-template
```

准备好 `config.yaml` 后，启动代理服务：

```bash
./api-proxy -config config.yaml
```

**其他命令行参数：**
- `./api-proxy -version` ：打印当前程序的版本并退出。
- `./api-proxy -print-template` ：在控制台打印默认的配置文件模板。

## 使用 Docker 运行

你可以十分方便地通过 Docker 运行它。运行容器时，请将宿主机的配置文件挂载到容器内部的 `/app` 目录下，并映射所需的端口（假设你的代理服务监听 8080 端口）：

```bash
docker run -d \
  --name my-api-proxy \
  -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  api-proxy:latest
```

> **注意：** 
> 1. 请根据你实际 `config.yaml` 中的 `Listen` 端口修改上面命令中的 `-p 8080:8080`。
> 2. 如果你的代码已经推送到 GitHub，GitHub Actions 会自动打包并推送镜像到 GitHub Container Registry。你可以直接将 `api-proxy:latest` 替换为形如 `ghcr.io/<你的用户名>/api-proxy:latest` 的远程镜像地址来运行。

## 如何编译与构建

本项目使用 Go 语言开发。请确保本地已经安装了 Go (>= 1.26) 环境。

### 1. 编译程序

你可以在根目录下使用标准的 `go build` 命令进行源码编译。如果你希望注入版本号并减小二进制体积，可以使用以下命令：

```bash
# 关闭 CGO 并注入版本信息
CGO_ENABLED=0 go build -ldflags="-s -w -X 'main.Version=v1.0.0'" -o api-proxy .
```

### 2. 本地构建 Docker 镜像

本项目已经包含了多阶段构建的 `Dockerfile`。如果你想自行从源码构建镜像：

```bash
docker build -t api-proxy:latest .
```
