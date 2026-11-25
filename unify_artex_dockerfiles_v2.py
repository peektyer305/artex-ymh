#!/usr/bin/env python3
"""Unify verified artex-ymh Dockerfiles and migrate source-build references."""

import argparse
import difflib
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


class Stop(Exception):
    """The checkout has an unsupported build configuration."""


CATALOG_LINES = ('COPY packaging/tools-manifest.json /app/packaging/tools-manifest.json', 'ENV ARTEX_TOOLS_MANIFEST=/app/packaging/tools-manifest.json')

PREBUILT_SIGNATURE = '7576f2849da923a032ddf5940d5474d92747197ddf6cba5e6aa7220021094c97'

SOURCE_SIGNATURE = '923748a3543b9f71749710fd88f6c0b7c21b269bf3c3d61b7c086721dcc8300c'

UNIFIED = r"""# syntax=docker/dockerfile:1
# artex-unified-dockerfile-v1
# --target source builds the application from source.
# The default prebuilt target packages dist/<TARGETARCH>/artex.
# Both targets share the former Dockerfile.source runtime.

FROM node:22-alpine AS fe
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci
COPY web/ ./
RUN npm run build:static

FROM golang:1.26-alpine AS gobuild
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p server/webui/dist
COPY --from=fe /src/web/out/ server/webui/dist/
RUN CGO_ENABLED=0 go build -tags embedui -trimpath -ldflags "-s -w" -o /out/artex ./cmd/artex

FROM python:3.12-slim-bookworm AS runtime
ARG TARGETARCH
ARG APT_MIRROR=
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG NODE_DIST_BASE=https://nodejs.org/dist
ARG NODE_VERSION=22.14.0
ARG PLAYWRIGHT_DOWNLOAD_HOST=
RUN if [ -n "$APT_MIRROR" ]; then \
      sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    fi \
    && apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      build-essential \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
RUN set -e; \
    case "$TARGETARCH" in \
      amd64) NODE_ARCH=x64 ;; \
      arm64) NODE_ARCH=arm64 ;; \
      *) echo "Unsupported Node architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL -o "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" "${NODE_DIST_BASE}/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz"; \
    curl -fsSL "${NODE_DIST_BASE}/v${NODE_VERSION}/SHASUMS256.txt" \
      | grep "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" | sha256sum -c -; \
    tar -xzf "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" -C /usr/local --strip-components=1; \
    rm -f "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz"
RUN npm config set registry "$NPM_REGISTRY" \
    && npm install -g @playwright/mcp@0.0.80 @playwright/cli@0.1.19 playwright@1.63.0 \
    && playwright-cli --help \
    && if [ -n "$PLAYWRIGHT_DOWNLOAD_HOST" ]; then \
         PLAYWRIGHT_DOWNLOAD_HOST="$PLAYWRIGHT_DOWNLOAD_HOST" playwright install --with-deps chromium; \
       else \
         playwright install --with-deps chromium; \
       fi \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY start.sh /app/start.sh
RUN chmod +x /app/start.sh
COPY skills/ /app/skills/
COPY packaging/tools-manifest.json /app/packaging/tools-manifest.json
ENV ARTEX_TOOLS_MANIFEST=/app/packaging/tools-manifest.json
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

FROM runtime AS source
COPY --from=gobuild /out/artex /app/artex
RUN chmod +x /app/artex

FROM runtime AS prebuilt
ARG TARGETARCH
COPY dist/${TARGETARCH}/artex /app/artex
RUN chmod +x /app/artex
"""

GUIDE = r"""<!-- artex-unified-build-targets-v1 -->
# Docker のビルド対象

Dockerfile は1つです。ソースから作る場合は source ターゲットを指定します。
旧ソース用 Dockerfile を参照していた Compose は、Dockerfile と target: source に変更しています。
ビルド済みバイナリを使う場合は prebuilt ターゲットを指定します。省略時も prebuilt です。

ソースからビルドする場合

    docker build --target source -t artex-ymh:local .

dist/<TARGETARCH>/artex を配置してから、ビルド済みバイナリを組み込む場合

    docker build --target prebuilt -t artex-ymh:prebuilt .

両方のターゲットは、旧ソース版の実行環境を共有します。
旧 Dockerfile の NodeSource 20.x は、Node.js 22.14.0 の tarball に切り替わります。
パッケージ構成も旧ソース版に揃うため、prebuilt 側にも build-essential が含まれます。
Node.js のアーカイブは TARGETARCH に応じて amd64/arm64 を選択します。
一覧ファイルは /app/packaging/tools-manifest.json に配置します。

この変更スクリプトは Docker コマンドを実行しません。
実際のイメージビルドとアプリの起動は別途確認してください。
リポジトリの外にある Ansible などの設定が旧ソース用 Dockerfile を参照している場合は、
Dockerfile と source ターゲットに変更する必要があります。
"""

README_MARKER = '<!-- artex-unified-build-targets-link -->'

README_NOTE = r"""<!-- artex-unified-build-targets-link -->
## Docker のビルド対象

ソース版とビルド済みバイナリ版の選択方法は [DOCKER_BUILD_TARGETS.md](DOCKER_BUILD_TARGETS.md) を参照してください。
"""

def normalized(raw):
    text = raw.decode("utf-8-sig")
    if "\r\n" in text and "\n" in text.replace("\r\n", ""):
        raise Stop("A target file has mixed line endings.")
    return text.replace("\r\n", "\n")


def signature(text):
    lines = [
        line.rstrip() for line in text.splitlines()
        if line.strip() and not line.lstrip().startswith("#")
        and line.strip() not in CATALOG_LINES
    ]
    return hashlib.sha256("\n".join(lines).encode()).hexdigest()


def read_regular(path):
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise Stop(f"Expected a regular file: {path}")
    return path.read_bytes() if path.exists() else None


def encode(text, original):
    if original and b"\r\n" in original:
        text = text.replace("\n", "\r\n")
    return text.encode("utf-8-sig" if original and original.startswith(b"\xef\xbb\xbf") else "utf-8")


def tracked_paths(root):
    result = subprocess.run(
        ["git", "ls-files", "-z"], cwd=root, check=False, capture_output=True,
    )
    if result.returncode:
        raise Stop("Run this script from a Git checkout, or specify --repo.")
    return set(result.stdout.decode("utf-8", "surrogateescape").split("\0")) - {""}


def update_yaml_builds(text, path):
    lines = text.splitlines(keepends=True)
    pattern = re.compile(
        r"^(?P<indent> +)(?:dockerfile|file):[ \t]*"
        r"(?P<quote>['\"]?)(?:\./)?Dockerfile\.source(?P=quote)[ \t]*(?:#[^\n]*)?\n?$"
    )
    edits = []
    for index, line in enumerate(lines):
        match = pattern.fullmatch(line)
        if not match:
            continue
        level = len(match["indent"])
        start, end = index, index + 1
        while start > 0:
            previous = lines[start - 1]
            if previous.strip() and not previous.lstrip().startswith("#"):
                if len(previous) - len(previous.lstrip(" ")) < level:
                    break
            start -= 1
        while end < len(lines):
            current = lines[end]
            if current.strip() and not current.lstrip().startswith("#"):
                if len(current) - len(current.lstrip(" ")) < level:
                    break
            end += 1
        targets = [
            j for j in range(start, end)
            if re.match(r"^" + " " * level + r"target:", lines[j])
        ]
        if len(targets) > 1:
            raise Stop(f"{path}: more than one target in a build block.")
        if targets and not re.fullmatch(
            " " * level + r"target:[ \t]*['\"]?source['\"]?[ \t]*(?:#[^\n]*)?\n?",
            lines[targets[0]],
        ):
            raise Stop(f"{path}: the source build already selects another target.")
        replacement = line.replace("Dockerfile.source", "Dockerfile").rstrip("\n") + "\n"
        if not targets:
            replacement += match["indent"] + "target: source\n"
        edits.append((index, replacement))
    for index, replacement in reversed(edits):
        lines[index] = replacement
    return "".join(lines)


def update_commands(text, path):
    lines = []
    file_flag = re.compile(
        r"(?P<flag>--file(?:=|[ \t]+)|-f[ \t]+)"
        r"(?P<quote>['\"]?)(?:\./)?Dockerfile\.source(?P=quote)"
    )
    for line in text.splitlines(keepends=True):
        if "Dockerfile.source" in line and file_flag.search(line):
            command = re.search(r"\bdocker[ \t]+(?:buildx[ \t]+)?build\b", line)
            if not command or re.search(r"--target(?:=|[ \t]+)", line):
                raise Stop(f"{path}: review its multiline/custom build or existing --target.")
            line = file_flag.sub(lambda m: m["flag"] + m["quote"] + "Dockerfile" + m["quote"], line)
            command = re.search(r"\bdocker[ \t]+(?:buildx[ \t]+)?build\b", line)
            line = line[:command.end()] + " --target source" + line[command.end():]
        lines.append(line)
    return "".join(lines)


def update_reference(text, path):
    text = update_commands(update_yaml_builds(text, path), path)
    if Path(path).name == "install.sh":
        # This is an installer message, not a Dockerfile argument.
        text = re.sub(
            r'(?m)^([ \t]*info[ \t]+)"ローカルの Dockerfile\.source からイメージをビルドして起動します…"[ \t]*$',
            r'\1"Dockerfile の source ターゲットからイメージをビルドして起動します…"',
            text,
        )
    if path.endswith(".md"):
        text = re.sub(r"(\]\([^\n)]*)Dockerfile\.source(?=[)#])", r"\1Dockerfile", text)
        return text.replace("Dockerfile.source", "Dockerfile (target: source)")
    lines = []
    for line in text.splitlines(keepends=True):
        if line.lstrip().startswith(("#", "//", "rem ", "REM ")):
            line = line.replace("Dockerfile.source", "Dockerfile (target: source)")
        lines.append(line)
    result = "".join(lines)
    if "Dockerfile.source" in result:
        numbers = ", ".join(str(i) for i, line in enumerate(result.splitlines(), 1) if "Dockerfile.source" in line)
        raise Stop(f"{path}: unsupported source-build reference at line(s) {numbers}. No files were written.")
    return result


def plan(root):
    paths = tracked_paths(root)
    before = read_regular(root / "Dockerfile")
    source = read_regular(root / "Dockerfile.source")
    if before is None:
        raise Stop("Dockerfile was not found.")
    if normalized(before) != UNIFIED:
        if signature(normalized(before)) != PREBUILT_SIGNATURE:
            raise Stop("Dockerfile has unsupported instruction changes.")
        if source is None or signature(normalized(source)) != SOURCE_SIGNATURE:
            raise Stop("Dockerfile.source has unsupported instruction changes.")
    elif source is not None and signature(normalized(source)) != SOURCE_SIGNATURE:
        raise Stop("An additional, customized Dockerfile.source exists.")
    if read_regular(root / "packaging/tools-manifest.json") is None:
        raise Stop("packaging/tools-manifest.json is missing.")
    replacements = {"Dockerfile": UNIFIED, "Dockerfile.source": None}
    paths.update(p.name for p in root.glob("*compose*.y*ml") if p.is_file())
    for path in sorted(paths):
        if path in replacements:
            continue
        candidate = root / path
        supported = candidate.suffix.lower() in (".md", ".sh", ".bat", ".ps1", ".yml", ".yaml", ".hcl") or candidate.name == "Makefile"
        if not supported or not candidate.is_file():
            continue
        text = normalized(read_regular(candidate))
        if "Dockerfile.source" in text:
            replacements[path] = update_reference(text, path)
    guide_before = read_regular(root / "DOCKER_BUILD_TARGETS.md")
    if guide_before is not None and normalized(guide_before) != GUIDE:
        raise Stop("DOCKER_BUILD_TARGETS.md already contains different content.")
    replacements["DOCKER_BUILD_TARGETS.md"] = GUIDE
    readme = read_regular(root / "README.md")
    if readme is not None:
        value = replacements.get("README.md", normalized(readme))
        if README_MARKER not in value:
            value = value.rstrip("\n") + "\n\n" + README_NOTE
        replacements["README.md"] = value
    changes = []
    for relative, value in replacements.items():
        path = root / relative
        previous = read_regular(path)
        after = None if value is None else encode(value, previous)
        if after != previous:
            changes.append((path, previous, after))
    return changes


def preview(root, changes):
    for path, before, after in changes:
        print("".join(difflib.unified_diff(
            normalized(before).splitlines(keepends=True) if before is not None else [],
            normalized(after).splitlines(keepends=True) if after is not None else [],
            fromfile="a/" + path.relative_to(root).as_posix() if before is not None else "/dev/null",
            tofile="b/" + path.relative_to(root).as_posix() if after is not None else "/dev/null",
        )), end="")


def apply_changes(root, changes):
    with tempfile.TemporaryDirectory(prefix=".unified-docker-", dir=root) as directory:
        prepared = []
        for index, (path, before, after) in enumerate(changes):
            draft = None
            if after is not None:
                draft = Path(directory) / str(index)
                draft.write_bytes(after)
                draft.chmod((path.stat().st_mode & 0o777) if before is not None else 0o644)
            prepared.append((path, draft, before, after))
        if any(read_regular(path) != before for path, _, before, _ in prepared):
            raise Stop("A target file changed during preparation. Run again.")
        done = []
        try:
            for path, draft, before, after in prepared:
                if after is None:
                    path.unlink()
                else:
                    os.replace(draft, path)
                done.append((path, before, after))
        except OSError:
            for path, before, after in reversed(done):
                if read_regular(path) == after:
                    if before is None:
                        path.unlink()
                    else:
                        path.write_bytes(before)
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", action="version", version="unify_artex_dockerfiles v2")
    parser.add_argument("--repo", type=Path, default=Path.cwd(), help="Repository root directory")
    parser.add_argument("--apply", action="store_true", help="Write the refactor; do not execute Docker")
    args = parser.parse_args()
    try:
        root = args.repo.resolve()
        changes = plan(root)
        if not changes:
            print("The Dockerfile unification is already applied.")
            return 0
        print("Runtime: former Dockerfile.source; Node 22.14.0 for both build targets.")
        preview(root, changes)
        if args.apply:
            apply_changes(root, changes)
            print("Updated: " + ", ".join(str(p.relative_to(root)) for p, _, _ in changes))
        else:
            print("Preview only. Add --apply to write the changes.")
        return 0
    except (Stop, OSError, UnicodeError) as error:
        print(f"Stopped: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
