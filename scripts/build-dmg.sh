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
#   4. 產生圖示：優先使用預建 .icns，否則從 SVG 轉換（需 rsvg-convert + iconutil）
#   5. 建立含 Applications 捷徑的暫存目錄
#   6. 使用 hdiutil 產出 DMG
#
# 環境需求（僅 SVG 轉換時）：
#   - rsvg-convert（brew install librsvg）
#   - iconutil（macOS 內建）

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

# --- 4. 產生圖示 ---
# 優先使用預建 .icns，否則從 SVG 自動轉換
ICNS_PATH="${PROJECT_ROOT}/macos/radb.icns"
SVG_PATH="${PROJECT_ROOT}/assets/RADB Icon.svg"

if [ -f "${ICNS_PATH}" ]; then
    echo "使用預建圖示：${ICNS_PATH}"
    cp "${ICNS_PATH}" "${APP_DIR}/Contents/Resources/radb.icns"
elif [ -f "${SVG_PATH}" ] && command -v rsvg-convert &>/dev/null; then
    echo "從 SVG 轉換圖示..."
    ICONSET_DIR="${STAGING_DIR}/radb.iconset"
    mkdir -p "${ICONSET_DIR}"

    # macOS iconset 需要的所有尺寸（名稱 → 像素）
    declare -A ICON_SIZES=(
        ["icon_16x16.png"]=16
        ["icon_16x16@2x.png"]=32
        ["icon_32x32.png"]=32
        ["icon_32x32@2x.png"]=64
        ["icon_128x128.png"]=128
        ["icon_128x128@2x.png"]=256
        ["icon_256x256.png"]=256
        ["icon_256x256@2x.png"]=512
        ["icon_512x512.png"]=512
        ["icon_512x512@2x.png"]=1024
    )

    for NAME in "${!ICON_SIZES[@]}"; do
        SIZE="${ICON_SIZES[$NAME]}"
        rsvg-convert -w "${SIZE}" -h "${SIZE}" "${SVG_PATH}" -o "${ICONSET_DIR}/${NAME}"
    done

    # iconutil 將 .iconset 目錄轉為 .icns（macOS 內建工具）
    iconutil -c icns "${ICONSET_DIR}" -o "${APP_DIR}/Contents/Resources/radb.icns"
    echo "圖示轉換完成"
else
    echo "警告：找不到圖示檔案或 rsvg-convert，將產出無圖示的 .app"
fi

# --- 5. 建立 DMG 暫存目錄（含 Applications 捷徑） ---
DMG_STAGING="${STAGING_DIR}/dmg"
mkdir -p "${DMG_STAGING}"
cp -r "${APP_DIR}" "${DMG_STAGING}/"
ln -s /Applications "${DMG_STAGING}/Applications"

# --- 6. 產出 DMG ---
hdiutil create \
    -volname "radb" \
    -srcfolder "${DMG_STAGING}" \
    -ov \
    -format UDZO \
    "${OUTPUT_DMG}"

# --- 清理 ---
rm -rf "${STAGING_DIR}"

echo "DMG 已產出：${OUTPUT_DMG}"
