package main

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/web"
)

// webListen is replaceable for the command integration test (port zero).
var webListen = net.Listen

func cmdWeb(ctx context.Context, args []string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	w := c.WebSettings()
	f := fs("web")
	f.StringVar(&w.Listen, "listen", w.Listen, "")
	if err := f.Parse(args); err != nil {
		return usageErr("web: %v", err)
	}
	if f.NArg() != 0 {
		return usageErr("web takes only --listen")
	}
	if err := config.ValidateWebListen(w.Listen); err != nil {
		return err
	}
	ln, err := webListen("tcp", w.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "horch web listening on http://%s (Ctrl-C stops the web server)\n", ln.Addr())
	return web.Serve(ctx, ln, web.New(web.SocketClient{Socket: cli.Paths.Sock}, w))
}
