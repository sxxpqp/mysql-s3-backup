# Docker / Kubernetes 部署

支持单次备份和 cron 定时备份。BACKUP_INTERVAL 留空时备份一次；默认 0 2 * * * 表示每天北京时间 02:00 执行，启动等待下一次计划时间。BACKUP_TIMEOUT=2h 是单次备份超时，与调度时间独立。没有 Web 页面或监听端口。以下命令在 Linux/Bash 执行，先进入仓库根目录。

## 1. 填写配置

```sh
cp .env.example .env
chmod 600 .env
vi .env
```

`.env` 每行 `KEY=value`，不加引号。将数据库地址、账号密码、S3 endpoint、桶名和 S3 密钥替换为真实值。存储桶需提前创建，账号需有上传权限。

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

`MYSQL_DATABASES` 留空代表全部数据库。MySQL/S3 必须能从容器或 Pod 网络访问；容器内 `127.0.0.1` 指向容器自身。Kubernetes 跨 namespace 数据库可用 `mysql.database.svc.cluster.local`。`.env` 包含运行凭据，不要提交 Git；GitHub 的 `ALIYUN_*` 仅用于构建镜像，不能代替这些参数。

## 2. Docker 单次备份

镜像地址为 `registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest`。私有仓库先交互登录：

```sh
docker login registry.cn-hangzhou.aliyuncs.com
export BACKUP_IMAGE='registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest'
docker pull "$BACKUP_IMAGE"
docker run --rm --env-file .env -e BACKUP_INTERVAL= "$BACKUP_IMAGE"
echo $?
```

看到 `backup uploaded: s3://...` 且退出码为 0 即上传成功，然后到桶里确认文件。备份临时文件使用容器磁盘，需保证空间足够。

## 3. Docker Compose

需要 Docker Compose 2.30+（使用 raw env_file 保留密码中的 `$` 等字符）。沿用上面的 `.env`，Compose 默认使用上述镜像，可通过 `BACKUP_IMAGE` 覆盖：

```sh
docker compose pull
docker compose up -d
docker compose logs -f backup
```

配置文件为根目录 `compose.yaml`。默认 restart 为 unless-stopped，程序内部按标准五字段 cron 调度，不需要宿主机 crontab。`.env` 中表达式不加引号：

```dotenv
BACKUP_TIMEOUT=2h
BACKUP_INTERVAL=0 2 * * *
BACKUP_TIMEZONE=Asia/Shanghai
```

- `0 2 * * *`：每天 02:00。
- `0 */6 * * *`：每天 00:00、06:00、12:00、18:00。
- `*/30 * * * *`：每半小时。
- `0 2 * * 1`：每周一 02:00。

启动后日志会打印下一次执行时间。备份串行执行，耗时跨过的计划点会跳过；失败后等待下一个计划点，不补跑。默认北京时间，S3 日期目录和文件时间也使用 BACKUP_TIMEZONE。兼容旧的 `24h` 时长格式，旧格式会启动立即备份，之后按完成时间计算间隔。

修改 `.env` 后执行 `docker compose up -d --force-recreate`。立即手动备份：`docker compose run --rm -e BACKUP_INTERVAL= backup`。停止定时容器：`docker compose down`。临时文件使用挂载到 /tmp 的数据卷，需要足够磁盘空间。

## 4. Kubernetes Deployment 常驻备份

使用 `deploy/k8s/deployment.yaml`，无需 CronJob。默认 `BACKUP_INTERVAL=0 2 * * *`，`BACKUP_TIMEZONE=Asia/Shanghai`，每天北京时间 02:00 执行。程序启动或 Pod 重启后等待下一个计划点，不立即备份。失败记录日志并等待下一计划点，同一个进程内不会重叠执行。保持 `replicas: 1`，使用 Recreate 更新策略减少更新期间重复备份。

先填写 `.env`（默认全部数据库），创建 namespace 和运行参数 Secret：

```sh
kubectl create namespace backup --dry-run=client -o yaml | kubectl apply -f -
kubectl -n backup create secret generic mysql-s3-backup-env --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
```

私有镜像需要 `registry-credentials`。先执行 `docker login registry.cn-hangzhou.aliyuncs.com`。若本机 Docker 配置直接包含 auths 凭据（而非仅有 credsStore），执行：

```sh
kubectl -n backup create secret generic registry-credentials --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson="$HOME/.docker/config.json" --dry-run=client -o yaml | kubectl apply -f -
```

使用凭据助手时请使用平台提供的镜像拉取 Secret 并修改 `imagePullSecrets`；公开镜像可删除该字段。

等待本次代码的镜像 CI 发布成功，然后部署：

```sh
kubectl apply --dry-run=server -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl -n backup rollout status deployment/mysql-s3-backup --timeout=180s
kubectl -n backup logs -f deployment/mysql-s3-backup
```

镜像默认 `registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest`，拉取策略 Always。生产环境可改为本次 CI 发布的固定 SHA 标签。当前镜像仅支持 amd64。临时空间默认 10Gi，按备份大小调整 emptyDir、ephemeral-storage 与节点容量。

Deployment 显式设置 BACKUP_INTERVAL 和 BACKUP_TIMEZONE，优先于 Secret 同名值。修改计划：编辑 deployment.yaml 中的 value 再 apply，或执行：

```sh
kubectl -n backup set env deployment/mysql-s3-backup BACKUP_INTERVAL='0 */6 * * *'
```

更新数据库/S3 参数：修改 `.env` 后重新创建/更新 Secret，再重启 Deployment。重启后会打印新的下次执行时间：

```sh
kubectl -n backup create secret generic mysql-s3-backup-env --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
kubectl -n backup rollout restart deployment/mysql-s3-backup
```

如果之前部署过 CronJob，先暂停它，等已有 Job 完成，再删除 CronJob 并启动 Deployment，避免两套任务同时执行：

```sh
kubectl -n backup patch cronjob mysql-s3-backup --type=merge -p '{"spec":{"suspend":true}}'
kubectl -n backup get jobs,pods
# 确认旧备份已结束后执行
kubectl -n backup delete cronjob mysql-s3-backup
```

暂停/恢复 Deployment（恢复后等待下一个计划时间）：

```sh
kubectl -n backup scale deployment/mysql-s3-backup --replicas=0
kubectl -n backup scale deployment/mysql-s3-backup --replicas=1
```

失败排查：

```sh
kubectl -n backup get pods -l app=mysql-s3-backup
kubectl -n backup describe pod <pod-name>
kubectl -n backup logs -f deployment/mysql-s3-backup
```

`ImagePullBackOff` 检查镜像地址和拉取 Secret；`mysqldump failed` 检查数据库网络、权限、密码与客户端兼容性；上传失败检查 S3 参数与桶权限。Pod Running 仅代表调度进程在运行，备份是否成功以 `backup uploaded` 日志及桶内文件为准。

## 日志与时区 / Logs and timezone

日志时间与计划时间统一使用 BACKUP_TIMEZONE（默认 Asia/Shanghai），使用带偏移的 ISO 时间，例如 `2026-10-31T12:00:00+08:00`。旧版日志前缀使用容器 UTC 时间，因此 04:00 与计划中的 12:00+08:00 是同一时刻，不是备份延迟。

Log timestamps, scheduled execution times, backup date folders and backup filenames all use BACKUP_TIMEZONE. A round uses one shared start timestamp; filenames include nanoseconds and UTC offset to reduce collisions. Startup probe timestamps remain UTC.

每次备份打印开始、数据库范围、超时、dump/压缩开始与耗时、压缩文件字节数和 MiB、上传目标与耗时、整体结果与总耗时，以及下次备份时间和等待时长。不打印数据库密码、S3 密钥或 SQL 内容。日志按阶段输出，大型 dump 运行期间不持续打印百分比。

Each backup logs the start, database scope, timeout, dump/compression duration, compressed size, upload destination and duration, total result and elapsed time, plus the next scheduled run. Credentials and SQL contents are not logged; progress is reported by stage, not as a continuous percentage.

## 逐库备份 / One file per database

MYSQL_DATABASES 留空时，使用 mysql 客户端自动查询当前账号可见的库，包括 mysql，跳过 information_schema、performance_schema、sys；需要账号有查看和备份相关库的权限。填写 `app1 app2` 则只备份指定库，每个库单独 mysqldump、gzip 并上传。每个文件包含该库的 CREATE DATABASE、USE 和数据，可单独恢复。不会将多个库合并到一个 SQL 文件。

With MYSQL_DATABASES empty, discover visible databases (including mysql, excluding virtual schemas); otherwise back up the space-separated selection. Each database gets a separate compressed SQL object. One database failure does not stop the remaining databases; the round fails if any database fails. BACKUP_TIMEOUT limits the entire round, not each database separately.

路径示例 / Example object paths:

- s3://rook/mysql/20261031/app1-20261031T120000.123456789+0800.sql.gz
- s3://rook/mysql/20261031/app2-20261031T120000.123456789+0800.sql.gz

每轮按库串行执行并及时删除该库临时文件。日志显示库名、进度、各库结果，以及总数、成功、失败、跳过数量。各库快照时间不同，不保证跨库同一时点一致性。不要对正在执行的定时任务再同时手动触发备份。

## 启动连接检查 / Startup connection checks

每次启动（含单次模式）都先执行连接检查，通过后才开始备份或进入定时调度：

1. MySQL：使用相同的地址、用户和密码登录，执行 SELECT 1，确认网络、认证和查询可用。
2. S3：向 `S3_PREFIX/.connection-checks/<UTC时间>.txt` 上传很小的探针，验证桶存在及当前密钥在该路径具有写权限。不会创建存储桶，不需要额外 ListBucket 权限。
3. 尝试删除刚创建的探针。没有删除权限时打印 WARNING 并继续，探针会保留，可手动清理或设置生命周期规则。

STARTUP_CHECK_TIMEOUT 默认 30s，覆盖上述全部步骤。MySQL/S3 登录或上传检查失败时退出码为 1，不进入等待计划时间的状态；Compose 和 Deployment 将根据重启策略重试。日志会显示失败步骤与耗时，不输出密码或密钥。

Every startup checks MySQL authentication with SELECT 1 and uploads a small S3 probe before entering the schedule. Failed connection/upload checks exit with status 1. Probe cleanup is best effort; missing delete permission produces a warning. STARTUP_CHECK_TIMEOUT bounds the entire check sequence (default 30s).

检查只证明当时连接和探针写入正常；不保证所有数据库表、视图、例程的 dump 权限，也不保证未来网络或每个数据库目录的桶策略可用，实际备份结果仍以逐库上传日志为准。