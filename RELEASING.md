# Eylu 发版流程

本文供 Eylu 仓库维护者使用，说明如何通过 Git 标签触发 GitHub Actions，自动完成质量检查、跨平台构建、签名和 GitHub Release 发布。

## 1. 发布约定

版本遵循 SemVer，标签必须带 `v` 前缀，并指向 `main` 分支历史中的提交。

| 类型 | 标签示例 | GitHub Release 状态 | 用途 |
|---|---|---|---|
| Alpha | `v1.1.0-alpha.1` | Pre-release | 早期开发验证 |
| Beta | `v1.1.0-beta.1` | Pre-release | 功能基本完成后的公开测试 |
| RC | `v1.1.0-rc.1` | Pre-release | 正式发布候选版本 |
| Stable | `v1.1.0` | 正式 Release | 稳定版本 |

同一版本通常按以下顺序递进：

```text
v1.1.0-alpha.1
v1.1.0-alpha.2
v1.1.0-beta.1
v1.1.0-rc.1
v1.1.0-rc.2
v1.1.0
```

正式版本号按变更类型递增：

- `PATCH`：兼容性问题修复，例如 `v1.1.0 -> v1.1.1`。
- `MINOR`：向后兼容的新功能，例如 `v1.1.1 -> v1.2.0`。
- `MAJOR`：包含破坏性兼容变更，例如 `v1.2.0 -> v2.0.0`。

## 2. 发布前检查

同步远端状态，并确认本地 `HEAD` 与 `origin/main` 指向同一提交：

```bash
git fetch origin main --tags --prune
git status --short --branch
git rev-parse HEAD origin/main
```

执行本地质量检查：

```bash
go mod verify
go test ./...
go vet ./...
go run ./scripts/generate-third-party-notices -check
goreleaser check
```

完整 CI 还会执行三平台 smoke test、race detector、格式检查、Staticcheck 和 `actionlint`。Windows 本地执行 race detector 需要启用 CGO 并安装 `gcc`。

需要预览最终归档时执行：

```bash
goreleaser release --snapshot --clean --skip=sign
```

快照只写入本地 `dist/`，不会创建 GitHub Release。

## 3. 发布版本

以 `v1.1.0-rc.1` 为例创建 annotated tag：

```bash
git tag -a v1.1.0-rc.1 -m "Release v1.1.0-rc.1"
git show --no-patch --pretty=fuller v1.1.0-rc.1
git push origin v1.1.0-rc.1
```

推送标签后，`.github/workflows/release.yml` 自动执行以下阶段：

1. 校验标签是否符合 SemVer，并确认标签提交属于 `main` 历史。
2. 复用 `.github/workflows/ci.yml`，执行 Linux、Windows、macOS 测试和质量检查。
3. 使用 GoReleaser 创建草稿 Release，并构建六个平台归档。
4. 生成 SHA-256 校验文件，并使用 Cosign keyless 为校验文件生成 Sigstore bundle。
5. 公开仓库额外生成 GitHub Artifact Attestation；个人账号私有仓库自动跳过此步骤。
6. 所有步骤成功后，将草稿公开为正式 Release 或 Pre-release。

## 4. 发布产物

GoReleaser 生成以下文件，其中 Windows 使用 ZIP，其余平台使用 tar.gz：

```text
Eylu_<version>_Windows_amd64.zip
Eylu_<version>_Windows_arm64.zip
Eylu_<version>_Linux_amd64.tar.gz
Eylu_<version>_Linux_arm64.tar.gz
Eylu_<version>_Darwin_amd64.tar.gz
Eylu_<version>_Darwin_arm64.tar.gz
Eylu_<version>_checksums.txt
Eylu_<version>_checksums.txt.sigstore.json
```

每个平台归档只包含 `eylu` 或 `eylu.exe`。GitHub 还会提供 Source code ZIP 和 tar.gz，因此 Release 页面通常显示 10 个资产。

Release notes 由 GoReleaser 根据上一个标签之后的 Git 提交生成，并排除 `docs:`、`test:`、`chore:` 和合并提交。

## 5. 发布后验证

在 GitHub Release 页面确认：

- 标签和目标提交正确。
- 预览版显示 `Pre-release`。
- 六个平台归档、校验文件和 Sigstore bundle 均存在。
- Release 工作流中的所有 job 均成功。

下载同一版本的全部构建产物后校验 SHA-256：

```bash
sha256sum -c Eylu_1.1.0-rc.1_checksums.txt
```

验证校验文件的 Sigstore bundle：

```bash
cosign verify-blob \
  --bundle Eylu_1.1.0-rc.1_checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/xnqycs/Eylu/.github/workflows/release.yml@refs/tags/v1.1.0-rc.1" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  Eylu_1.1.0-rc.1_checksums.txt
```

运行对应平台的程序并确认构建元数据：

```bash
./eylu version
```

Windows：

```powershell
.\eylu.exe version
```

输出应包含版本、提交、构建日期及 `built_by=goreleaser`。

## 6. 失败处理

### 瞬时网络或 Runner 故障

在 GitHub Actions 中执行 `Re-run failed jobs`。GoReleaser 配置了 `replace_existing_draft: true`，重跑时会替换同标签的旧草稿。

### 工作流配置问题

当 Release 仍为未公开草稿时，可以修复工作流并让同一版本标签指向新提交：

```bash
git add .github/workflows/release.yml
git commit -m "fix(release): 修复发版流程"
git push origin main

git push origin :refs/tags/v1.1.0-rc.1
git tag -d v1.1.0-rc.1
git tag -a v1.1.0-rc.1 -m "Release v1.1.0-rc.1"
git push origin v1.1.0-rc.1
```

新标签会触发一次新的 Release 工作流，并自动替换旧草稿。

### 已公开版本发现问题

保留已经公开的 Release 和标签，创建递增版本：

- 预览版问题：发布 `rc.2`、`beta.2` 或相应的下一序号。
- 稳定版兼容性修复：递增 `PATCH`。
- 新增兼容功能：递增 `MINOR`。
- 破坏性兼容变更：递增 `MAJOR`。

### 标签指向错误提交

先确认 Release 的公开状态。未公开草稿可按“工作流配置问题”流程移动标签；已公开版本使用新版本号发布修正内容。

## 7. 关键配置

| 文件 | 职责 |
|---|---|
| `.github/workflows/release.yml` | 标签触发、版本校验、CI 门禁、构建和公开 Release |
| `.github/workflows/ci.yml` | 三平台测试、race detector 和静态分析 |
| `.goreleaser.yaml` | 构建矩阵、归档命名、校验、签名和草稿配置 |
| `.github/release.yml` | GitHub 自动生成 Release notes 时的分类配置 |
| `CHANGELOG.md` | 面向维护者和用户的长期变更记录 |
