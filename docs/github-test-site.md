# GitHub 测试站

仓库提供 GitHub Pages 静态交互演示，方便打开网址查看页面及体验套餐、下单和模拟开通流程。演示始终显示身份提示，仅使用当前标签页内存中的模拟数据，不连接控制端、不保存真实账号、不发送邮件、不收款、不注册真实节点。请不要输入真实密码、商户密钥或个人资料。

GitHub Pages 不能运行 Go 控制端或 PostgreSQL。这个站点用于界面体验，不能作为真实账号安全、SMTP、付款、代理流量或生产环境的验收证据。完整平台测试继续由独立的 `Verify platform` 工作流运行真实 API、PostgreSQL 和客户端集成测试；持续运行的完整测试平台仍需单独部署后端和数据库。

## 自动发布

[发布工作流](../.github/workflows/pages.yml)在 `main` 的前端相关变更后自动运行，也可在仓库 Actions 中选择 `Publish GitHub test site` → `Run workflow`。只发布可信 `main` 分支的构建，不接收 PR 构建产物；部署权限限定为读取代码、Pages 写入和 OIDC。

工作流会尝试启用 Pages 的 Actions 构建。如果仓库或组织策略不允许自动启用，仓库管理员需在 **Settings → Pages → Build and deployment → Source** 选择 **GitHub Actions**，然后重新运行。私有仓库还需当前 GitHub 计划支持 Pages；此流程不会将仓库改为公开。

部署完成后，在 Actions 的部署任务输出或仓库 `github-pages` Environment 中查看准确网址。`wjgppx/xui` 的默认项目站点通常为 `https://wjgppx.github.io/xui/`；必须以成功部署输出及实际访问结果为准，不能仅因工作流已提交就认为网址生效。

Git 推送成功也不代表当前云工作区能读取 GitHub API 或访问测试站。如果工作区网络仅允许 Git 传输而拒绝 `api.github.com` 或 `wjgppx.github.io`，需要先在环境设置中允许所需的精确域名并保存发布，再核实 Actions 和 HTTPS 访问结果。网络返回 Forbidden 不能直接推定仓库令牌无权限，也不能把尚未核实的默认网址标为已经上线。

构建后先在独立静态预览端口执行演示浏览器验收，再上传 `web/dist`。部署后还会探测实际 HTTPS 入口，并对发布网址再执行相同浏览器用例；这些用例只修改各自标签页中的内存数据，不改远程共享数据。

构建只上传 `web/dist`，不上传数据库、`.env.local`、备份、源代码中的服务器私钥或后端程序。演示模式只在构建时通过 `VITE_DEMO_MODE=true` 启用，普通生产构建不启用模拟数据。`VITE_BASE_PATH` 来自 Pages 的实际项目路径，默认生产部署仍为 `/`。

## 本地预览

```bash
cd web
VITE_DEMO_MODE=true VITE_BASE_PATH=/xui/ npm run build
VITE_BASE_PATH=/xui/ npx vite preview --host 127.0.0.1 --port 5174 --strictPort
```

打开 `http://127.0.0.1:5174/xui/`。刷新会重新初始化内存演示，所有数据都不是其他访客共享的真实业务数据。模拟流程和只读展示的具体范围由页面提示说明。

部署普通完整平台时，重新执行不带上述演示变量的 `npm --prefix web run build`，按[生产部署说明](production-deployment.md)连接真实控制端。不要把演示构建当作生产用户入口。
