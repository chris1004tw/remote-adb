#!/bin/bash
# build-dmg.sh — 將編譯好的 radb binary 包成 macOS .app bundle 並產出 DMG
#
# 用法：
#   scripts/build-dmg.sh <binary-path> <version> <output-dmg>
#
# 範例：
#   scripts/build-dmg.sh dist/radb v1.0.0 radb-v1.0.0-darwin-arm64.dmg
#
# 流程：
#   1. 建立 radb.app bundle 目錄結構（Contents/MacOS + Contents/Resources）
#   2. 將 Info.plist 模板中的版本號替換後寫入
#   3. 複製 binary
#   4. 使用 actool 編譯 Icon Composer .icon bundle 為 .icns + Assets.car
#   5. 使用 create-dmg 產出帶背景圖與圖示定位的 DMG
#
# 環境需求：
#   - Xcode command line tools（actool）
#   - create-dmg（brew install create-dmg）

set -euo pipefail

BINARY_PATH="$1"
VERSION="$2"
OUTPUT_DMG="$3"

# 專案根目錄（以此腳本位置推算）
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

APP_NAME="radb.app"
STAGING_DIR="$(mktemp -d)"
APP_DIR="${STAGING_DIR}/${APP_NAME}"

# --- 1. 建立 .app bundle 目錄結構 ---
mkdir -p "${APP_DIR}/Contents/MacOS"
mkdir -p "${APP_DIR}/Contents/Resources"

# --- 2. 處理 Info.plist（替換版本號） ---
# 移除 v 前綴（例如 v1.0.0 → 1.0.0），CFBundleVersion 不接受 v 前綴
CLEAN_VERSION="${VERSION#v}"
sed "s/\${VERSION}/${CLEAN_VERSION}/g" "${PROJECT_ROOT}/macos/Info.plist" > "${APP_DIR}/Contents/Info.plist"

# --- 3. 複製 binary ---
cp "${BINARY_PATH}" "${APP_DIR}/Contents/MacOS/radb"
chmod +x "${APP_DIR}/Contents/MacOS/radb"

# --- 4. 使用 actool 編譯圖示 ---
# 從 Icon Composer 的 .icon bundle 編譯出 .icns（舊版 fallback）和 Assets.car（macOS 26+ 分層圖示）
ICON_SOURCE="${PROJECT_ROOT}/macos/RADB.icon"
ICON_BUILD_DIR="${STAGING_DIR}/icon"
mkdir -p "${ICON_BUILD_DIR}"

xcrun actool "${ICON_SOURCE}" \
    --app-icon RADB \
    --compile "${ICON_BUILD_DIR}" \
    --platform macosx \
    --minimum-deployment-target 11.0 \
    --output-partial-info-plist "${ICON_BUILD_DIR}/partial.plist" \
    --output-format human-readable-text \
    --errors --warnings

cp "${ICON_BUILD_DIR}/RADB.icns" "${APP_DIR}/Contents/Resources/radb.icns"
cp "${ICON_BUILD_DIR}/Assets.car" "${APP_DIR}/Contents/Resources/Assets.car"

# --- 5. 使用 create-dmg 產出帶背景圖與圖示定位的 DMG ---
# 背景圖 654×422，箭頭置中於 (327, 200)
# radb.app (167) 與 Applications (487) 各距中心 160px，確保對稱
BACKGROUND="${PROJECT_ROOT}/assets/dmg-background.png"

# create-dmg 不接受已存在的輸出檔，先移除
rm -f "${OUTPUT_DMG}"

create-dmg \
    --volname "radb" \
    --background "${BACKGROUND}" \
    --window-pos 200 120 \
    --window-size 654 422 \
    --icon-size 128 \
    --icon "radb.app" 167 200 \
    --hide-extension "radb.app" \
    --app-drop-link 487 200 \
    --no-internet-enable \
    "${OUTPUT_DMG}" \
    "${APP_DIR}"

# --- 清理 ---
rm -rf "${STAGING_DIR}"

echo "DMG 已產出：${OUTPUT_DMG}"
