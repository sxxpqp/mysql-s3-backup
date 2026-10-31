# Docker / Kubernetes 部署

支持单次备份和内部定时备份。BACKUP_INTERVAL 留空时，备份一次后退出；设为 24h 等正时长时，启动立即备份，之后每次备份尝试结束等待该间隔再执行。没有 Web 页面或监听端口。以下命令在 Linux/Bash 执行，先进入仓库根目录。

## 1. 填写配置

```sh
cp .env.example .env
chmod 600 .env
vi .env
```

`.env` 每行 `KEY=value`，不加引号。将数据库地址、账号密码、S3 endpoint、桶名和 S3 密钥替换为真实值。存储桶需提前创建，账号需有上传权限。

```dotenv
MYSQL_HOST=10.0.0.10
MYSQL_PORT=3306
MYSQL_USER=backup
MYSQL_PASSWORD=replace-me
# 留空备份全部数据库；指定多个数据库时用空格分隔，例如 app1 app2
MYSQL_DATABASES=
S3_ENDPOINT=https://s3.example.com
S3_BUCKET=db-backup
S3_PREFIX=mysql/production
AWS_REGION=us-east-1
AWS_ACCESS_KEY_ID=replace-me
AWS_SECRET_ACCESS_KEY=replace-me
BACKUP_TIMEOUT=2h
```

`MYSQL_DATABASES` 留空代表全部数据库。MySQL/S3 必须能从容器或 Pod 网络访问；容器内 `127.0.0.1` 指向容器自身。Kubernetes 跨 namespace 数据库可用 `mysql.database.svc.cluster.local`。`.env` 包含运行凭据，不要提交 Git；GitHub 的 `ALIYUN_*` 仅用于构建镜像，不能代替这些参数。

## 2. Docker 单次备份

镜像地址为 `registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest`。私有仓库先交互登录：

```sh
docker login registry.cn-hangzhou.aliyuncs.com
export BACKUP_IMAGE='registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest'
docker pull "$BACKUP_IMAGE"
docker run --rm --env-file .env "$BACKUP_IMAGE"
echo $?
```

看到 `backup uploaded: s3://...` 且退出码为 0 即上传成功，然后到桶里确认文件。备份临时文件使用容器磁盘，需保证空间足够。

## 3. Docker Compose

需要 Docker Compose 2.30+（使用 raw env_file 保留密码中的 `$` 等字符）。沿用上面的 `.env`，Compose 默认使用上述镜像，可通过 `BACKUP_IMAGE` 覆盖：

```sh
docker compose pull
docker compose run --rm backup
```

配置文件为根目录 `compose.yaml`，临时数据卷挂载 `/tmp`，备份成功或失败都会清理本次临时文件。修改 `.env` 后重新执行上述 run 命令即生效。需要定时执行时，由宿主机 cron 调度；例如 `crontab -e` 添加以下一行（修改目录及镜像地址；时间遵循宿主机时区）：

```cron
0 2 * * * cd /opt/mysql-s3-backup && BACKUP_IMAGE=registry.cn-hangzhou.aliyuncs.com/sxxpqp/mysql-s3-backup:latest /usr/bin/docker compose run --rm -T backup >> /var/log/mysql-s3-backup.log 2>&1
```

## 4. Kubernetes Deployment 常驻备份

使用 `deploy/k8s/deployment.yaml`，无需 CronJob。程序启动立即备份一次，每次尝试结束后等待 `BACKUP_INTERVAL` 再执行，默认 `24h`。失败记录日志并等待下一次重试，同一个进程内不会重叠执行。Pod 重启会立即执行一次；这不是每天固定钟点调度。保持 `replicas: 1`，使用 Recreate 更新策略减少更新期间重复备份。

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

Deployment 显式设置 `BACKUP_INTERVAL=24h`，优先于 Secret 中的同名值。修改间隔：编辑 deployment.yaml 中的 value（如 `6h`、`12h`），再 apply；也可以直接执行：

```sh
kubectl -n backup set env deployment/mysql-s3-backup BACKUP_INTERVAL=6h
```

更新数据库/S3 参数：修改 `.env` 后重新创建/更新 Secret，再重启 Deployment。重启后立即备份一次：

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

暂停/恢复 Deployment（恢复后立即执行备份）：

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