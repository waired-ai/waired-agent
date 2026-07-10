package wgnet

import "encoding/base64"

func base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
