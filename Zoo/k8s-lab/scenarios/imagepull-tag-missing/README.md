# imagepull-tag-missing

症状 ImagePullBackOff；镜像 `busybox:1.36-nope`——仓库能连通，tag 不存在，
报错原文里带镜像引用的 `not found`（具体文案随 CRI/registry 变，判分不绑它）。

人工复核：`kubectl -n diag-lab describe pod badtag` 的事件里能看到拉取失败原文；
`kubectl -n diag-lab get pod badtag -o jsonpath='{.status.containerStatuses[0].state.waiting.message}'`
与事件 message 一致。

期望结论：`root_cause_category=imagepull_tag_missing`，不能归到"私有仓库缺凭据"——
本场景没有配任何 imagePullSecrets，但失败的真正原因是 tag 不存在（凭据问题会有 401/403 字样）。

前置：本场景的判据依赖 registry 可达（报错里是 tag 不存在的 404）。若本机到 Docker Hub 的网络
不通或抖动，报错会变成 EOF/timeout 这类网络错误，那时模型判成 registry 不可达是对的，不该记它错——
评测前先确认 `kubectl -n diag-lab describe pod` 里的报错确实是 404 类。
