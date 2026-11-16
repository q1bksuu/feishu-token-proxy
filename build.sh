#!/usr/bin/env bash
# 交叉编译并打包。用法：./build.sh [os/arch ...]，例如 ./build.sh windows/amd64；不带参数时构建 targets 里的全部平台。
# 每个平台输出目录 dist/feishu-token-proxy-<版本>-<os>-<arch>/（程序、config.example.json、README.md、LICENSE）
# 及同名压缩包：windows 为 .zip，其余为 .tar.gz。版本取环境变量 VERSION，为空时取 git describe。
set -euo pipefail
cd "$(dirname "$0")"

name=feishu-token-proxy
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
  targets=(windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)
fi

mkdir -p dist
for target in "${targets[@]}"; do
  os=${target%/*}
  arch=${target#*/}
  pkg="$name-$version-$os-$arch"
  bin=$name
  [ "$os" = windows ] && bin=$name.exe

  rm -rf "dist/$pkg" "dist/$pkg.zip" "dist/$pkg.tar.gz"
  mkdir -p "dist/$pkg"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "dist/$pkg/$bin" .
  cp config.example.json README.md LICENSE "dist/$pkg/"

  if [ "$os" = windows ]; then
    (cd dist && zip -qr9 "$pkg.zip" "$pkg")
    echo "dist/$pkg.zip"
  else
    tar -C dist -czf "dist/$pkg.tar.gz" "$pkg"
    echo "dist/$pkg.tar.gz"
  fi
done
