minikube start && minikube addons enable metrics-server   # 启用后等 1 分钟让首次采样落库
bash Zoo/k8s-lab/smoke.sh up
bash Zoo/k8s-lab/smoke.sh test
bash Zoo/k8s-lab/smoke.sh down