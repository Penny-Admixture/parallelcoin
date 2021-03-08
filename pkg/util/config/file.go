package cfgutil

import (
	"os"
)

// FileExists reports whether the named file or directory exists.
func FileExists(filePath string) (bool, error) {
	_, e := os.Stat(filePath)
	if e != nil  {
		err.Ln(err)
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
