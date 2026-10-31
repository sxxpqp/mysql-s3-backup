# mysql-s3-backup

使用 Go 实现的 MySQL → S3 备份工具。通过 Docker Compose 或 Kubernetes Deployment 常驻运行，由程序内部按 cron 调度，无需宿主机 crontab 或 Kubernetes CronJob。

- 每库独立生成 `.sql.gz`，逐库压缩上传，可以分别恢复。
- 默认发现账号可见的全部数据库，包含 `mysql`，跳过 `information_schema`、`performance_schema`、`sys`。
- 启动检查 MySQL 登录、查询和 S3 写入；检查成功后才进入调度。
- 调度、日志、备份目录、文件名和启动探针统一使用配置时区，默认北京时间。
- 日志包含逐库进度、文件大小、各阶段耗时和结果，不输出密码、密钥或 SQL 内容。

## 镜像与项目文件

```text
registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest
```

当前 CI 构建 **linux/amd64** 镜像，内置 MySQL 8.4 客户端。其他服务端版本需验证客户端兼容性。

| 文件 | 用途 |
| --- | --- |
| [.env.example](.env.example) | 带中英文注释的配置模板 |
| [compose.yaml](compose.yaml) | Docker Compose 常驻备份 |
| [deploy/k8s/deployment.yaml](deploy/k8s/deployment.yaml) | Kubernetes 单副本 Deployment |
| [Dockerfile](Dockerfile) | Go 编译与运行镜像 |
| [.github/workflows/docker.yaml](.github/workflows/docker.yaml) | 测试、镜像构建与发布 |

以下部署命令使用 Linux/Bash，在仓库根目录执行。

## 1. 填写配置

```bash
git clone https://github.com/sxxpqp/mysql-s3-backup.git
cd mysql-s3-backup
cp .env.example .env
chmod 600 .env
vi .env
```

修改数据库、S3 地址及账号密钥，提前创建存储桶。容器/Pod 必须能访问 MySQL 和 S3；容器内 `127.0.0.1` 指向容器自身。跨 namespace 的数据库 Service 可使用 `mysql.database.svc.cluster.local` 等完整地址。

`.env` 使用 `KEY=value`，值不加引号，注释放在单独一行，兼容 Docker、Compose raw env_file 和 Kubernetes Secret 导入。

```dotenv
# 数据库地址：填写容器/Pod 能访问的域名或 IP / MySQL hostname or IP reachable from the container/Pod
MYSQL_HOST=mysql
# 数据库端口 / MySQL port
MYSQL_PORT=3306
# 备份用户：需要所选数据库的备份权限 / MySQL user with backup permissions
MYSQL_USER=backup
# 数据库密码：替换为真实值 / MySQL password; replace with your actual value
MYSQL_PASSWORD=replace-me
# 留空自动发现全部可见库（含 mysql，跳过系统视图库），每库独立文件；多个库用空格分隔 / Empty = discover visible databases, including mysql, excluding virtual schemas; one file per database
MYSQL_DATABASES=

# S3 服务地址：包含 http:// 或 https://，不包含桶名和路径 / S3 endpoint URL, without bucket or object prefix
S3_ENDPOINT=https://s3.amazonaws.com
# 存储桶名称：需提前创建，例如 rook / Existing bucket name, e.g. rook
S3_BUCKET=rook
# 桶内路径前缀：类似文件夹，不是另一个桶；默认 mysql / Object key prefix inside the bucket, like a folder; default mysql
# 示例路径 / Example: s3://rook/mysql/YYYYMMDD/<database>-<backup timestamp>.sql.gz
S3_PREFIX=mysql
# S3 区域：按服务商配置 / S3 region configured by your provider
AWS_REGION=us-east-1
# S3 访问密钥 ID：不是 MySQL 用户名 / S3 access key ID, separate from MySQL credentials
AWS_ACCESS_KEY_ID=replace-me
# S3 访问密钥：替换为真实值，不要提交 Git / S3 secret access key; never commit actual credentials
AWS_SECRET_ACCESS_KEY=replace-me
# 临时凭据令牌：仅使用 STS 临时凭据时启用 / Optional session token for temporary STS credentials
# AWS_SESSION_TOKEN=replace-me

# 整轮备份超时：所有库的发现、dump、压缩、上传共用此时限 / Whole backup round timeout shared by discovery and all database dumps/uploads
BACKUP_TIMEOUT=2h
# 启动连接检查总超时：MySQL 登录/查询与 S3 探针上传清理 / Startup timeout shared by MySQL login/query and S3 probe upload/cleanup
STARTUP_CHECK_TIMEOUT=30s
# 五字段 cron：分 时 日 月 周；每天 02:00；留空仅运行一次 / Five-field cron: minute hour day month weekday; empty = run once
# 其他示例 / More examples: */30 * * * * = every 30 minutes; 0 */6 * * * = at 00:00, 06:00, 12:00, 18:00
BACKUP_INTERVAL=0 2 * * *
# 调度、日志和备份文件时区：默认北京时间 / Timezone for schedule, logs, date folders and backup filenames
BACKUP_TIMEZONE=Asia/Shanghai
```

### S3_BUCKET 和 S3_PREFIX 的区别

- `S3_BUCKET=rook`：存储桶名称，需要提前创建。
- `S3_PREFIX=mysql`：桶内路径前缀，类似文件夹，不是另一个桶。

备份路径为 `s3://桶名/前缀/年月日/库名-备份时间.sql.gz`：

```text
s3://rook/mysql/20261031/app1-20261031T122000.123456789+0800.sql.gz
s3://rook/mysql/20261031/app2-20261031T122000.123456789+0800.sql.gz
```

**日期和文件名以整轮备份开始时间为准**，所有库共享本轮时间戳。例如 23:59 开始、次日 00:05 结束，仍归入开始当天。文件名保留小数秒和时区偏移，减少同名覆盖。特殊库名会进行 URL 路径段编码。

## 2. Docker Compose 启动

需要 Docker Compose **2.30+**，`env_file.format: raw` 用于保留密码中的 `$` 等字符。

```bash
# 私有镜像需登录，公开镜像可跳过
docker login registry.cn-hangzhou.aliyuncs.com
docker compose pull
docker compose up -d
docker compose logs -f backup
```

Compose 默认使用上述镜像，`restart: unless-stopped` 用于异常退出或宿主机重启后恢复。定时备份由 Go 程序内部执行。

更新镜像或修改 `.env` 后重新创建容器：

```bash
docker compose pull
docker compose up -d --force-recreate
```

立即手动执行一次，不启动定时循环：

```bash
docker compose run --rm -e BACKUP_INTERVAL= backup
```

手动执行前确认没有其他备份在运行；多个容器之间不会互斥。停止常驻容器：

```bash
docker compose down
```

如需固定版本，在 `.env` 添加 `BACKUP_IMAGE=registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:<成功发布的完整commit-SHA>`，替换占位符后重新创建容器。

### Docker 单次运行

```bash
docker run --rm --env-file .env -e BACKUP_INTERVAL= \
  registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest
```

## 3. cron 定时与超时

```dotenv
# 每天北京时间 02:00 / Daily at 02:00 in the configured timezone
BACKUP_INTERVAL=0 2 * * *
BACKUP_TIMEZONE=Asia/Shanghai
# 整轮备份超时 / Whole backup round timeout
BACKUP_TIMEOUT=2h
# 启动连接检查总超时 / Startup check timeout
STARTUP_CHECK_TIMEOUT=30s
```

cron 使用标准五字段：**分 时 日 月 周**，不包含秒。

| 表达式 | 执行时间 |
| --- | --- |
| `*/10 * * * *` | 每小时的 00、10、20、30、40、50 分 |
| `*/30 * * * *` | 每小时的 00、30 分 |
| `0 2 * * *` | 每天 02:00 |
| `0 */6 * * *` | 每天 00:00、06:00、12:00、18:00 |
| `0 2 * * 1` | 每周一 02:00 |

- cron 模式启动检查后等待下一个计划点，不立即备份。12:16 启动、配置 `*/10 * * * *`，下次是 12:20。
- `BACKUP_INTERVAL` 留空只运行一次，完成后退出。使用单次命令执行，避免配合 Compose 自动重启策略造成反复备份。
- 兼容 `24h` 等正时长：启动立即备份，之后每轮结束等待指定时长，与固定钟点 cron 不同。
- 同一进程串行执行，运行期间错过的计划点会跳过，不补跑；失败后等下一个计划点，不立即重试整轮。
- `BACKUP_TIMEOUT` 限制所有库共用的整轮发现、dump、压缩、上传时间，独立于调度间隔。

## 4. Kubernetes Deployment 部署

使用 [deployment.yaml](deploy/k8s/deployment.yaml)，无需 CronJob。默认单副本、Recreate 更新策略、amd64 节点、Always 拉取策略。保持单副本，避免重复备份。

### 4.1 创建 envFrom 引用的运行参数 Secret

`envFrom.secretRef.name: mysql-s3-backup-env` 表示：把同一 namespace 中名为 `mysql-s3-backup-env` 的 Secret 内所有键值注入容器环境变量。Deployment 不会自动创建这个 Secret，必须先创建，否则 Pod 会报 `CreateContainerConfigError`。

先在项目根目录准备 `.env`，填写真实 MySQL/S3 参数。已有 `.env` 时不要用模板覆盖：

```bash
# 仅首次配置时执行
cp .env.example .env
chmod 600 .env
vi .env
```

再创建 namespace，并从 `.env` 导入 Secret。以下命令既可首次创建，也可更新已有 Secret：

```bash
kubectl create namespace backup --dry-run=client -o yaml | kubectl apply -f -
kubectl -n backup create secret generic mysql-s3-backup-env \
  --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
```

确认 Secret 已创建（不输出其中的密码、密钥）：

```bash
kubectl -n backup get secret mysql-s3-backup-env
```

这个 Secret 存放 MySQL/S3 运行参数，与镜像拉取 Secret `registry-credentials` 是两个不同资源。若修改 namespace，Secret 和 Deployment 必须一起改到同一个 namespace。

### 4.2 创建镜像拉取 Secret 并部署

私有镜像需要同 namespace 的 `registry-credentials`。先执行 `docker login registry.cn-hangzhou.aliyuncs.com`；若 Docker 配置直接包含 auths 认证信息，可导入：

```bash
kubectl -n backup create secret generic registry-credentials \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$HOME/.docker/config.json" \
  --dry-run=client -o yaml | kubectl apply -f -
```

如果 Docker 使用 credsStore/凭据助手，仅导入配置文件不能提供认证。请使用集群平台创建的镜像拉取 Secret，并修改 YAML 的 imagePullSecrets 名称；公开镜像可移除该字段。

部署并查看日志：

```bash
kubectl apply --dry-run=server -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl -n backup rollout status deployment/mysql-s3-backup --timeout=180s
kubectl -n backup logs -f deployment/mysql-s3-backup
```

**Deployment 显式设置的 BACKUP_INTERVAL、BACKUP_TIMEZONE 优先于 Secret 同名值。** 修改计划请编辑 YAML 再 apply，或执行：

```bash
kubectl -n backup set env deployment/mysql-s3-backup \
  BACKUP_INTERVAL='*/10 * * * *' BACKUP_TIMEZONE=Asia/Shanghai
```

修改 `.env` 数据库/S3 参数后，更新 Secret 并重启：

```bash
kubectl -n backup create secret generic mysql-s3-backup-env \
  --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
kubectl -n backup rollout restart deployment/mysql-s3-backup
kubectl -n backup rollout status deployment/mysql-s3-backup --timeout=180s
```

更新 latest 镜像同样需要 rollout restart；生产可将 YAML 镜像固定为成功发布的完整 commit SHA 标签。重启会中断正在运行的备份，尽量在空闲时更新；cron 模式重启后等待下一计划点。

暂停/恢复：

```bash
kubectl -n backup scale deployment/mysql-s3-backup --replicas=0
kubectl -n backup scale deployment/mysql-s3-backup --replicas=1
```

如果以前部署了 CronJob，先暂停它、等旧 Job 结束，再删除 CronJob 后部署 Deployment，避免同时运行两套备份任务。

## 5. 启动检查与日志

每次启动，包括单次模式，都会依次：

1. 使用 MySQL 账号登录并执行 SELECT 1。
2. 向 `S3_PREFIX/.connection-checks/<配置时区时间>.txt` 上传小探针，验证桶及该路径写权限。
3. 尝试删除探针，然后进入备份或调度。

MySQL 登录/查询或 S3 上传失败时退出码为 1，由容器重启策略重试。删除失败只打印 WARNING 并继续，残留探针可手动或用生命周期规则清理；开启版本控制的桶还需考虑非当前版本。

启动检查不等于验证所有库、表、视图、例程的 dump 权限，也不保证后续网络和数据库文件路径的桶策略一定可用。

日志示例（省略部分行和字段）：

```text
2026-10-31T12:16:35+08:00 startup MySQL check passed: elapsed=195ms
2026-10-31T12:16:35+08:00 startup S3 probe cleanup passed
2026-10-31T12:16:35+08:00 next backup: 2026-10-31T12:20:00+08:00 wait=3m24s
2026-10-31T12:20:00+08:00 database backup started: database="app1" progress=1/2
2026-10-31T12:20:15+08:00 dump completed: database="app1" elapsed=15s compressed_bytes=849417 compressed_mib=0.81
2026-10-31T12:20:17+08:00 backup uploaded: s3://rook/mysql/20261031/app1-20261031T122000.123456789+0800.sql.gz (849417 bytes) upload_elapsed=2s
2026-10-31T12:20:30+08:00 backup summary: total=2 succeeded=2 failed=0 skipped=0
2026-10-31T12:20:30+08:00 backup completed: elapsed=30s
```

Pod Running 或容器运行仅代表进程存活；以 backup uploaded、本轮汇总和桶内对象判断成功。日志按阶段输出，大型 dump 运行期间没有持续百分比进度。

## 6. 数据库范围、权限与空间

- `MYSQL_DATABASES=`：自动查询账号可见库，包含 mysql，跳过三个系统视图库；看不到的库不会被发现。
- `MYSQL_DATABASES=app1 app2`：仅备份指定库，每库一个文件，不合并 SQL，重复库名会去重。
- 单库失败继续其他库，任意库失败则整轮失败。达到总超时后停止，未执行库计入 skipped，已上传文件保留。
- 文件包含该库的 CREATE DATABASE、USE 和数据，可单独恢复。各库依次建立快照，不保证跨库同一时点一致性。
- mysqldump 使用 single-transaction，面向 InnoDB；备份期间避免 DDL，非事务表需另行安排锁定。
- MySQL 账号需有对应库的读取、视图、触发器、例程及事件备份权限；包含 mysql 系统库时也需相应权限。
- S3 账号需有备份和探针路径的写权限，大文件分段上传还需相应 multipart 操作权限；删除权限用于探针清理。
- 每库先在临时磁盘完成 gzip，再上传；完成或失败后清理该库临时文件。Compose 挂载数据卷到 /tmp，Kubernetes emptyDir 默认上限 10Gi，按单库压缩大小及节点余量调整存储和资源 requests/limits。

## 7. 下载与恢复

从 S3 下载需要的单库文件，先校验 gzip，再在隔离测试实例恢复：

```bash
gzip -t app1-20261031T122000.123456789+0800.sql.gz
set -o pipefail
gzip -dc app1-20261031T122000.123456789+0800.sql.gz \
  | mysql -h test-mysql -P 3306 -u restore -p
```

文件自带库名，会创建/选择原库，并可能覆盖同名表。先在隔离环境验证，并定期执行实际恢复验证。程序不自动删除历史备份，保留时间由存储桶生命周期规则管理。

## 8. 常见问题

| 现象 | 排查方向 |
| --- | --- |
| 启动后只有 next backup | cron 等待下一计划点，正常运行 |
| 启动反复退出 | 查看启动检查失败步骤，检查 DNS、网络、凭据、桶写权限 |
| 探针清理 WARNING | 删除权限不足、保留策略或网络异常；上传通过可继续备份 |
| mysqldump failed | 检查库权限、客户端兼容性、连接和超时；stderr 未原样输出，可在受控终端复现 |
| upload backup 失败 | 检查 endpoint、区域、密钥、桶策略、容量和网络 |
| ImagePullBackOff | 检查镜像地址、拉取 Secret 与 namespace |
| 时间不符合预期 | 检查 BACKUP_TIMEZONE，当前日志与所有文件名统一使用配置时区 |
| 修改 .env 未生效 | Compose 重建容器；Kubernetes 更新 Secret 后重启；检查显式 env 覆盖 |
| 单次成功后不断重跑 | 单次模式配合自动重启；改用 run --rm，或设置 cron 常驻 |

## 9. 开发与镜像 CI

Go 版本以 [go.mod](go.mod) 为准，本地运行还需 mysql、mysqldump 客户端。可用 MYSQL_BIN、MYSQLDUMP_BIN 指定客户端路径，默认从 PATH 查找。

```bash
go test ./...
go vet ./...
docker build -t mysql-s3-backup:local .
```

GitHub Actions 在 main push、v* tag push 或手动触发时执行；Pull Request 执行测试检查。发布前运行 go test -race 和 go vet，随后构建并推送镜像。main 发布 latest 和完整 commit SHA 标签，tag 发布对应版本标签。

Actions Secrets 与 sxxpqp/docker_image_pusher 使用同名配置：

| Secret | 值/用途 |
| --- | --- |
| ALIYUN_REGISTRY | registry.cn-hangzhou.aliyuncs.com |
| ALIYUN_NAME_SPACE | sxxpqp |
| ALIYUN_REGISTRY_USER | 阿里云镜像仓库登录用户名 |
| ALIYUN_REGISTRY_PASSWORD | 阿里云镜像仓库登录密码 |

Secrets 不会跨仓库自动继承。CI 构建凭据与 .env 的 MySQL/S3 运行凭据分别配置，真实密钥不要提交 Git。
