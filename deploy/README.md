# 部署说明

完整配置、Docker Compose 与 Kubernetes Deployment 操作请查看 [项目 README](../README.md)。

Kubernetes 使用 Secret YAML 配置运行参数：

```bash
cp deploy/k8s/secret.yaml.example deploy/k8s/secret.yaml
chmod 600 deploy/k8s/secret.yaml
vi deploy/k8s/secret.yaml
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/secret.yaml
# 私有镜像需先配置 registry-credentials，详见项目 README
kubectl apply -f deploy/k8s/deployment.yaml
kubectl -n backup logs -f deployment/mysql-s3-backup
```

命令在项目根目录执行。修改 Secret 后需要 rollout restart；Deployment 的运行参数统一从 Secret 注入。真实 secret.yaml 已加入 .gitignore，切勿提交密钥。
