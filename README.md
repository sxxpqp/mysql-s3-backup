# mysql-s3-backup

Go 编写的 MySQL 备份工具：逐库调用 MySQL 8.4 `mysqldump`，每个库独立 gzip 压缩，成功后上传 S3 兼容存储（AWS S3、MinIO 等）。单次模式失败退出码为 1，不会将失败的 dump 上传为备份。临时文件自动清理，需要足够的临时磁盘空间。

## 运行

Docker Compose 与 Kubernetes Deployment 的完整配置和操作命令见 [部署说明](deploy/README.md)。

复制 `.env.example` 为 `.env` 并修改连接信息，然后运行：

```sh
docker run --rm --env-file .env -e BACKUP_INTERVAL= registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest
```

默认示例使用 cron 定时：`BACKUP_INTERVAL=0 2 * * *`、`BACKUP_TIMEZONE=Asia/Shanghai`，每天北京时间 02:00 执行，启动等待下一次计划点。`BACKUP_INTERVAL` 留空则运行一次；兼容旧的 `24h` 时长格式。定时模式失败后等待下一计划点；备份串行执行并跳过运行期间错过的计划点。Kubernetes 使用单副本 Deployment。`MYSQL_DATABASES` 用空格分隔，留空自动发现可见数据库并逐库备份（包含 mysql，跳过 information_schema、performance_schema、sys）；每库一个文件，单库失败继续其他库，整轮返回失败。`S3_ENDPOINT` 支持 HTTPS（默认）或显式 HTTP，使用 path-style 寻址。支持 `AWS_SESSION_TOKEN` 临时凭据。`BACKUP_TIMEOUT` 默认 `2h`，覆盖数据库发现及所有库 dump、压缩、上传的整轮过程。对象路径为 `S3_PREFIX/<数据库名>/YYYY/MM/DD/<UTC时间>.sql.gz`，默认前缀 `mysql`。

`MYSQL_USER` 需要选定数据库的读取、视图、触发器、例程与事件等相关备份权限。`--single-transaction` 用于 InnoDB 一致性备份；备份期间避免 DDL，非事务表需另行安排锁定。镜像自带 MySQL 8.4 客户端；其他服务端版本应先验证客户端兼容性。

## CI 镜像构建

main 分支 push、`v*` tag push 或手动触发 GitHub Actions。先执行测试和 vet，再构建 linux/amd64 镜像并推送阿里云。main 产出 `latest` 和 commit SHA 标签；tag 产出版本标签。

与 `sxxpqp/docker_image_pusher` 使用完全相同的 **GitHub Actions Secrets 名称**：

| Secret | 用途 |
|---|---|
| ALIYUN_REGISTRY | 镜像仓库地址，不带协议 |
| ALIYUN_NAME_SPACE | 仓库命名空间 |
| ALIYUN_REGISTRY_USER | 登录用户名 |
| ALIYUN_REGISTRY_PASSWORD | 登录密码 |

Secrets 不会跨仓库自动继承；在本仓库设置同名值，或配置允许本仓库访问的组织级 Secrets。运行时 MySQL/S3 环境变量见 `.env.example`，与构建凭据独立。

## 恢复验证

从 S3 下载文件后，在隔离测试数据库恢复：

```sh
gzip -dc backup.sql.gz | mysql -h test-mysql -u restore -p
```

请定期执行恢复验证。程序不自动删除历史对象，可用存储桶生命周期规则控制保留时间。

每个文件仅包含一个数据库的 CREATE DATABASE、USE 和该库数据，恢复时可分别导入。不同数据库是依次建立事务快照，不保证跨库同一时点一致性。库名在对象路径中按 URL path segment 编码。