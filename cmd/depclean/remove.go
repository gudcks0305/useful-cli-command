package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/useful-go/internal/rootfs"
)

// removeArtifactNoFollow deletes through os.Root handles anchored at the scan
// root. Concurrent symlink swaps cannot redirect traversal outside that root.
func removeArtifactNoFollow(rootPath string, dep FoundDependency) (retErr error) {
	rel, err := filepath.Rel(rootPath, dep.DepPath)
	if err != nil || !strictlyBelow(rootPath, dep.DepPath) {
		return errors.New("secure delete 대상이 검색 루트 밖이거나 루트 자체임")
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("검색 루트 handle 열기 실패: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("검색 루트 handle 닫기 실패: %w", err))
		}
	}()
	pathInfo, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("secure delete artifact Lstat 실패: %w", err)
	}
	if dep.identity == nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() || !os.SameFile(dep.identity, pathInfo) {
		return errors.New("secure delete 직전 artifact identity 불일치")
	}

	target, err := root.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("secure delete artifact handle 열기 실패: %w", err)
	}
	targetInfo, err := target.Lstat(".")
	if err != nil {
		return errors.Join(
			fmt.Errorf("secure delete artifact handle 검사 실패: %w", err),
			closeArtifactRoot(target),
		)
	}
	if !os.SameFile(dep.identity, targetInfo) {
		return errors.Join(errors.New("artifact handle이 분석 시점 identity와 다름"), closeArtifactRoot(target))
	}
	metadata, err := rootfs.Inspect(target)
	if err != nil {
		return errors.Join(
			fmt.Errorf("secure delete 직전 artifact 재검사 실패: %w", err),
			closeArtifactRoot(target),
		)
	}
	if metadata.Size != dep.Size || !metadata.Latest.Equal(dep.LastAccess) {
		return errors.Join(
			errors.New("secure delete 직전 artifact 내용이 분석 시점과 다름"),
			closeArtifactRoot(target),
		)
	}
	if err := rootfs.RemoveContents(target); err != nil {
		return errors.Join(err, closeArtifactRoot(target))
	}
	if err := closeArtifactRoot(target); err != nil {
		return err
	}

	current, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("artifact 최종 identity 검사 실패: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(targetInfo, current) {
		return errors.New("내용 삭제 후 artifact 경로 identity가 변경됨")
	}
	if err := root.Remove(rel); err != nil {
		return fmt.Errorf("artifact 디렉터리 제거 실패: %w", err)
	}
	return nil
}

func closeArtifactRoot(root *os.Root) error {
	if err := root.Close(); err != nil {
		return fmt.Errorf("artifact handle 닫기 실패: %w", err)
	}
	return nil
}
