// Command govim is a Vim8 channel-based plugin, written in Go, to support the writing of Go code in Vim8
package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kr/pretty"
	"github.com/myitcv/govim"
	"github.com/myitcv/govim/cmd/govim/internal/jsonrpc2"
	"github.com/myitcv/govim/cmd/govim/internal/lsp/protocol"
	"github.com/myitcv/govim/cmd/govim/internal/span"
	"github.com/myitcv/govim/cmd/govim/types"
	"github.com/myitcv/govim/internal/plugin"
	"gopkg.in/tomb.v2"
)

var (
	fTail = flag.Bool("tail", false, "whether to also log output to stdout")
)

func main() {
	os.Exit(main1())
}

func main1() int {
	switch err := mainerr(); err {
	case nil:
		return 0
	case flag.ErrHelp:
		return 2
	default:
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
}

func mainerr() error {
	flag.Parse()

	if sock := os.Getenv("GOVIMTEST_SOCKET"); sock != "" {
		ln, err := net.Listen("tcp", sock)
		if err != nil {
			return fmt.Errorf("failed to listen on %v: %v", sock, err)
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				return fmt.Errorf("failed to accept connection on %v: %v", sock, err)
			}

			go func() {
				if err := launch(conn, conn); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}()
		}
	} else {
		return launch(os.Stdin, os.Stdout)
	}
}

func launch(in io.ReadCloser, out io.WriteCloser) error {
	defer out.Close()

	d := newDriver()

	nowStr := time.Now().Format("20060102_1504_05.000000000")
	tf, err := ioutil.TempFile("", "govim_"+nowStr+"_*")
	if err != nil {
		return fmt.Errorf("failed to create log file")
	}
	defer tf.Close()

	var log io.Writer = tf
	if *fTail {
		log = io.MultiWriter(tf, os.Stdout)
	}

	if os.Getenv("GOVIMTEST_SOCKET") != "" {
		fmt.Fprintf(os.Stderr, "New connection will log to %v\n", tf.Name())
	}

	g, err := govim.NewGoVim(d, in, out, log)
	if err != nil {
		return fmt.Errorf("failed to create govim instance: %v", err)
	}

	d.Kill(g.Run())
	return d.Wait()
}

type driver struct {
	*plugin.Driver

	gopls       *os.Process
	goplsConn   *jsonrpc2.Conn
	goplsCancel context.CancelFunc
	server      protocol.Server

	// buffers represents the current state of all buffers in Vim. It is only safe to
	// write and read to/from this map in the callback for a defined function, command
	// or autocommand.
	buffers map[int]*types.Buffer

	tomb tomb.Tomb

	// omnifunc calls happen in pairs (see :help complete-functions). The return value
	// from the first tells Vim where the completion starts, the return from the second
	// returns the matching words. This is by definition stateful. Hence we persist that
	// state here
	lastCompleteResults *protocol.CompletionList
}

type parseData struct {
	fset *token.FileSet
	file *ast.File
}

func newDriver() *driver {
	return &driver{
		Driver:  plugin.NewDriver("GOVIM"),
		buffers: make(map[int]*types.Buffer),
	}
}

func (d *driver) Init(g *govim.Govim) error {
	d.Driver.Govim = g
	d.ChannelEx(`augroup govim`)
	d.ChannelEx(`augroup END`)
	d.DefineFunction("Hello", []string{}, d.hello)
	d.DefineCommand("Hello", d.helloComm)
	d.DefineFunction("BalloonExpr", []string{}, d.balloonExpr)
	d.ChannelEx("set balloonexpr=GOVIMBalloonExpr()")
	d.DefineAutoCommand("", govim.Events{govim.EventBufReadPost, govim.EventBufNewFile}, govim.Patterns{"*.go"}, false, d.bufReadPost)
	d.DefineAutoCommand("", govim.Events{govim.EventTextChanged, govim.EventTextChangedI}, govim.Patterns{"*.go"}, false, d.bufTextChanged)
	d.DefineAutoCommand("", govim.Events{govim.EventBufWritePre}, govim.Patterns{"*.go"}, false, d.formatCurrentBuffer)
	d.DefineFunction("Complete", []string{"findarg", "base"}, d.complete)
	d.ChannelEx("set omnifunc=GOVIMComplete")

	goplsPath, err := installGoPls()
	if err != nil {
		return fmt.Errorf("failed to install gopls: %v", err)
	}

	gopls := exec.Command(goplsPath)
	out, err := gopls.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe for gopls: %v", err)
	}
	in, err := gopls.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe for gopls: %v", err)
	}
	if err := gopls.Start(); err != nil {
		return fmt.Errorf("failed to start gopls: %v", err)
	}
	d.tomb.Go(func() (err error) {
		if err = gopls.Wait(); err != nil {
			err = fmt.Errorf("got error running gopls: %v", err)
		}
		return
	})

	stream := jsonrpc2.NewHeaderStream(out, in)
	ctxt, cancel := context.WithCancel(context.Background())
	conn, server := protocol.NewClient(stream, d)
	go conn.Run(ctxt)

	d.gopls = gopls.Process
	d.goplsConn = conn
	d.goplsCancel = cancel
	d.server = server

	wd := d.ParseString(d.ChannelCall("getcwd", -1))
	initParams := &protocol.InitializeParams{
		InnerInitializeParams: protocol.InnerInitializeParams{
			RootURI: string(span.FileURI(wd)),
		},
	}
	g.Logf("calling protocol.Initialize(%v)", pretty.Sprint(initParams))
	initRes, err := server.Initialize(context.Background(), initParams)
	if err != nil {
		return fmt.Errorf("failed to initialise gopls: %v", err)
	}
	d.Logf("gopls init complete: %v", pretty.Sprint(initRes.Capabilities))

	return nil
}

func (d *driver) Shutdown() error {
	return nil
}

func installGoPls() (string, error) {
	// If we are being run as a plugin we require that it is somewhere within
	// the github.com/myitcv/govim module. That allows tests to work but also
	// the plugin itself when run from within plugin/govim.vim
	modlist := exec.Command("go", "list", "-m", "-f={{.Dir}}", "github.com/myitcv/govim")
	out, err := modlist.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to determine directory of github.com/myitcv/govim: %v", err)
	}

	gobin := filepath.Join(string(out), "cmd", "govim", ".bin")

	cmd := exec.Command("go", "install", "golang.org/x/tools/cmd/gopls")
	cmd.Env = append(os.Environ(), "GOBIN="+gobin)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run [%v] in %v: %v\n%s", strings.Join(cmd.Args, " "), gobin, err, out)
	}

	return filepath.Join(gobin, "gopls"), nil
}
