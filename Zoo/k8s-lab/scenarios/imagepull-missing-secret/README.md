# imagepull-missing-secret

症状 ImagePullBackOff；镜像指向 `ghcr.io/diag-lab-private/app:1`——registry 能连通，但匿名取 token
被拒：`failed to authorize: failed to fetch anonymous token: ... 403 Forbidden`。Pod 与 ServiceAccount
两处都没有 imagePullSecrets，所以"配了凭据就能拉"的结论成立。

人工复核：`kubectl -n diag-lab describe pod <pod>` 的事件里是 403 鉴权失败；
`kubectl -n diag-lab get pod <pod> -o jsonpath='{.spec.imagePullSecrets}'` 为空，
`kubectl -n diag-lab get sa default -o jsonpath='{.imagePullSecrets}'` 也为空。

期望结论：`root_cause_category=imagepull_missing_secret`，不能归到 tag 不存在或镜像名错——
那两类是"鉴权通过了、对象不在"（404 / manifest unknown / repository does not exist），
本场景的报错里只有鉴权失败，没有任何"对象不存在"的字样。

前置：依赖 ghcr.io 可达。若本机到 ghcr 的网络不通，报错会变成 EOF/timeout 这类网络错误，
那时模型判成 registry 不可达是对的，不该记它错——评测前先确认报错里确实是 403。
