// archive.go 負責從 .tar.gz 或 .zip 格式的 archive 中安全地提取 binary 檔案。
//
// 安全設計要點：
//   - 使用 knownBinaries 白名單，僅提取預期的執行檔，防止 archive 中夾帶惡意檔案
//   - 針對 path traversal 攻擊（zip slip / tar slip）進行防護，拒絕包含 ".." 的路徑
//   - 提取的檔案一律使用 filepath.Base() 取得純檔名，不保留 archive 內的目錄結構
package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// knownBinaries 是允許從 archive 中提取的 binary 名稱白名單。
// 設計意圖：即使 archive 中包含其他檔案（如 README、LICENSE 等），
// 也只會提取此白名單中的執行檔，降低供應鏈攻擊風險。
var knownBinaries = map[string]bool{
	"radb":     true, // Unix 平台
	"radb.exe": true, // Windows 平台
}

// ExtractArchive 解壓 archive 到 destDir，只提取已知的 binary 檔案。
// 根據副檔名自動判斷 tar.gz 或 zip 格式。
// 回傳已提取的檔案路徑列表。
func ExtractArchive(archivePath, destDir string) ([]string, error) {
	if strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz") {
		return extractTarGz(archivePath, destDir)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return nil, fmt.Errorf("不支援的 archive 格式: %s", archivePath)
}

// extractTarGz 解壓 .tar.gz 格式的 archive，從中提取白名單內的 binary。
func extractTarGz(archivePath, destDir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("開啟 archive 失敗: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("建立 gzip reader 失敗: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var extracted []string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("讀取 tar entry 失敗: %w", err)
		}

		// 只處理一般檔案（跳過目錄、symlink 等）
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)

		// tar slip 防護：拒絕路徑中包含 ".." 的 entry，
		// 避免惡意 archive 將檔案寫入 destDir 之外的目錄
		if strings.Contains(hdr.Name, "..") {
			continue
		}

		// 不在白名單中的檔案直接跳過
		if !knownBinaries[name] {
			continue
		}

		// 輸出時只保留檔名，不重建 archive 內的目錄結構
		destPath := filepath.Join(destDir, name)
		if err := writeFile(destPath, tr, hdr.FileInfo().Mode()); err != nil {
			return nil, err
		}
		extracted = append(extracted, destPath)
	}

	return extracted, nil
}

// extractZip 解壓 .zip 格式的 archive，從中提取白名單內的 binary。
// 主要用於 Windows 平台的 release archive。
func extractZip(archivePath, destDir string) ([]string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("開啟 zip 失敗: %w", err)
	}
	defer zr.Close()

	var extracted []string

	for _, zf := range zr.File {
		name := filepath.Base(zf.Name)

		// zip slip 防護：拒絕路徑中包含 ".." 的 entry，
		// 避免惡意 archive 將檔案寫入 destDir 之外的目錄
		if strings.Contains(zf.Name, "..") {
			continue
		}

		// 跳過目錄 entry
		if zf.FileInfo().IsDir() {
			continue
		}

		// 不在白名單中的檔案直接跳過
		if !knownBinaries[name] {
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("開啟 zip entry %q 失敗: %w", zf.Name, err)
		}

		destPath := filepath.Join(destDir, name)
		err = writeFile(destPath, rc, zf.Mode())
		rc.Close()
		if err != nil {
			return nil, err
		}
		extracted = append(extracted, destPath)
	}

	return extracted, nil
}

// writeFile 將 reader 的內容寫入指定路徑。
// mode 參數指定檔案權限；若為 0（例如 zip 格式中某些 entry 未保留權限），
// 則預設使用 0755（rwxr-xr-x），因為提取的對象是可執行的 binary，
// 必須具備執行權限才能正常運行。
func writeFile(path string, r io.Reader, mode os.FileMode) error {
	if mode == 0 {
		mode = 0755
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("建立檔案 %q 失敗: %w", path, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("寫入檔案 %q 失敗: %w", path, err)
	}
	return nil
}
