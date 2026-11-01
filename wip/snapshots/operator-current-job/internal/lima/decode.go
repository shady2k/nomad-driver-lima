package lima

import (
	"encoding/base64"
	"fmt"
)

func decode(s string) ([]byte, error) {
	if len(s) > 90000 {
		return nil, fmt.Errorf("guest log chunk exceeds limit")
	}
	return base64.StdEncoding.DecodeString(s)
}
