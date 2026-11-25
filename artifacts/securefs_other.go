//go:build !unix

package artifacts

import (
	"errors"
	"os"
)

// Refuse publication on platforms without the descriptor-relative no-follow
// implementation. Never fall back to privileged path-check-then-open copying.
var errUnsupported = errors.New("collected snapshots require a Unix server")

func openTaskDirectory(string) (*os.File, error)                  { return nil, errUnsupported }
func openChildDirectory(*os.File, string, bool) (*os.File, error) { return nil, errUnsupported }
func openRelativeFile(*os.File, string) (*os.File, error)         { return nil, errUnsupported }
func createChildFile(*os.File, string) (*os.File, error)          { return nil, errUnsupported }
func publishChildDirectory(*os.File, string, string) error        { return errUnsupported }
func removeChildDirectory(*os.File, string)                       {}
func removeKnownChildDirectory(*os.File, string) error            { return errUnsupported }
func safeRegular(os.FileInfo) bool                                { return false }
func metadataChanged(os.FileInfo, os.FileInfo) bool               { return true }
func protectDirectory(*os.File) error                             { return errUnsupported }
func protectFile(*os.File) error                                  { return errUnsupported }
