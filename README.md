## kube-term
 kubernetes web shell + web log

#### 功能

- 执行命令/查看日志
- 根据浏览器窗口自动调整 tty 大小
- 内嵌静态资源
- 支持心跳保活，自定义超时时常 (server -> client, server -> kubernetes)
- 优雅断开 shell（发送 Ctrl-C、Ctrl-D 等断开 shell 连接）
- 根据 kubeconfig 文件支持多集群
- 权限控制
