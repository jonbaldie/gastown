package acp

import (
	"encoding/json"
	"io"
)

func setProxyStreams(proxy *Proxy, in io.Reader, out io.Writer) {
	proxy.stdin = in
	proxy.stdout = out
	proxy.uiEncoder = json.NewEncoder(out)
}
