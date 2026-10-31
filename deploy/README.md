# Docker / Kubernetes 部署

这是单次备份任务，成功后容器退出（退出码 0）属于正常行为。没有 Web 页面、监听端口或常驻服务。以下命令在 Linux/Bash 执行，先进入仓库根目录。

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
MYSQL_DATABASES=app1 app2
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

## 4. Kubernetes 定时备份

使用 Kubernetes 1.27+，配置文件 `deploy/k8s/cronjob.yaml` 默认北京时间每天 02:00 执行。其中 `image` 已配置为本仓库镜像，并固定到 CI 成功发布的 commit SHA 标签。示例仅支持 amd64 节点，与当前 CI 镜像一致。`/tmp` 默认 10Gi，请按压缩备份大小调整 sizeLimit、ephemeral-storage requests/limits 与节点磁盘空间。

```sh
kubectl create namespace backup --dry-run=client -o yaml | kubectl apply -f -
kubectl -n backup create secret generic mysql-s3-backup-env --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
```

私有镜像仓库：在本机先执行 `docker login registry.cn-hangzhou.aliyuncs.com`，若 Docker 配置文件直接包含该仓库 auths 凭据（而不是仅配置 credsStore），可创建拉取 Secret：

```sh
kubectl -n backup create secret generic registry-credentials --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson="$HOME/.docker/config.json" --dry-run=client -o yaml | kubectl apply -f -
```

若 Docker 使用凭据助手，使用平台提供的镜像拉取 Secret 并修改 `imagePullSecrets` 的名称；公开镜像可删除 `imagePullSecrets`。

```sh
kubectl apply --dry-run=server -f deploy/k8s/cronjob.yaml
kubectl apply -f deploy/k8s/cronjob.yaml
kubectl -n backup get cronjob mysql-s3-backup
```

立即手动备份并查看日志：

```sh
JOB="mysql-s3-backup-manual-$(date +%s)"
kubectl -n backup create job "$JOB" --from=cronjob/mysql-s3-backup
kubectl -n backup get pods -l job-name="$JOB" -w
# Pod 启动后，在另一终端执行（JOB 替换为上一步实际名称）
kubectl -n backup logs -f job/"$JOB"
kubectl -n backup wait --for=condition=complete job/"$JOB" --timeout=7500s
```

`concurrencyPolicy: Forbid` 避免同一个 CronJob 的定时任务重叠，但不限制手动创建的 Job；手动触发前确认没有正在运行的备份。

修改配置：编辑 `.env`，重新执行创建/更新 `mysql-s3-backup-env` Secret 的命令，新 Job 会读取新值，正在运行的 Job 不变。修改时间或资源后重新 apply CronJob。暂停/恢复定时备份：

```sh
kubectl -n backup patch cronjob mysql-s3-backup --type=merge -p '{"spec":{"suspend":true}}'
kubectl -n backup patch cronjob mysql-s3-backup --type=merge -p '{"spec":{"suspend":false}}'
```

失败排查：

```sh
kubectl -n backup get jobs,pods
kubectl -n backup describe pod <pod-name>
kubectl -n backup logs <pod-name>
```

`ImagePullBackOff` 检查镜像地址和拉取 Secret；`mysqldump failed` 检查数据库网络、用户权限、密码与服务端兼容性；上传失败检查 S3 endpoint、密钥、桶名及桶权限。
