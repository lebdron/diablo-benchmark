package naptos

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"diablo-benchmark/core"
	"diablo-benchmark/util"

	aptosmodels "github.com/portto/aptos-go-sdk/models"
)

type moveCompiler struct {
	logger core.Logger
	base   string
}

func newMoveCompiler(logger core.Logger, base string) *moveCompiler {
	return &moveCompiler{
		logger: logger,
		base:   base,
	}
}

func (c *moveCompiler) compile(name string, owner *account) (*application, error) {
	var mdpath, scpath, scname, path string
	var parser *util.ServiceProcess
	var module, ctor, fun []byte
	var funcs map[string][]byte
	var infos []fs.FileInfo
	var info fs.FileInfo
	var err error

	c.logger.Debugf("compile contract '%s'", name)

	mdpath = c.base + "/" + name + "/module.move"
	c.logger.Debugf("  module path: '%s'", mdpath)
	module, err = c.compileModule(mdpath, owner)
	if err != nil {
		return nil, err
	}
	c.logger.Debugf("  module code is %d bytes", len(module))

	scpath = c.base + "/" + name + "/new.move"
	c.logger.Debugf("  constructor path: '%s'", scpath)
	ctor, err = c.compileScript(mdpath, scpath, owner)
	if err != nil {
		return nil, err
	}
	c.logger.Debugf("  constructor code is %d bytes", len(ctor))

	infos, err = ioutil.ReadDir(c.base + "/" + name)
	if err != nil {
		return nil, err
	}

	funcs = make(map[string][]byte)
	for _, info = range infos {
		scname = info.Name()

		if scname == "module.move" {
			continue
		}

		if scname == "new.move" {
			continue
		}

		if !strings.HasSuffix(scname, ".move") {
			continue
		}

		scpath = c.base + "/" + name + "/" + info.Name()
		c.logger.Debugf("  function path: '%s'", scpath)

		fun, err = c.compileScript(mdpath, scpath, owner)
		if err != nil {
			return nil, err
		}

		scname = scname[0:strings.LastIndex(scname, ".move")]
		funcs[scname] = fun
		c.logger.Debugf("  function '%s' code is %d bytes", scname,
			len(fun))
	}

	path = c.base + "/" + name + "/arguments"
	if !strings.HasPrefix(path, "/") {
		path = "./" + path
	}

	parser, err = util.StartServiceProcess(path)
	if err != nil {
		return nil, err
	}

	return &application{
		logger:     c.logger,
		moduleCode: module,
		ctorCode:   ctor,
		funcCodes:  funcs,
		parser:     parser,
		scanner:    bufio.NewScanner(parser),
		deployed:   false,
	}, nil
}

func (c *moveCompiler) compileSources(paths []string, owner *account) (string, error) {
	var cmd *exec.Cmd
	var tmp string
	var err error

	tmp, err = ioutil.TempDir("", "move")
	if err != nil {
		return "", err
	}

	args := make([]string, 0)
	args = append(args, "--addresses", "Owner="+owner.signer.ToHex())
	args = append(args, "--addresses", "Std=0x1")
	args = append(args, "--out-dir", tmp)
	args = append(args, paths...)
	cmd = exec.Command("move-build", args...)
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	return tmp, nil
}

func (c *moveCompiler) compileModule(path string, owner *account) ([]byte, error) {
	var out string
	var err error

	out, err = c.compileSources([]string{path}, owner)
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(out)

	return readFileIn(out+"/modules", ".mv")
}

func (c *moveCompiler) compileScript(module, script string, owner *account) ([]byte, error) {
	var out string
	var err error

	out, err = c.compileSources([]string{module, script}, owner)
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(out)

	return readFileIn(out+"/scripts", ".mv")
}

func readFileIn(path, suffix string) ([]byte, error) {
	var content []byte = nil
	var infos []fs.FileInfo
	var info fs.FileInfo
	var file *os.File
	var err error

	infos, err = ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, info = range infos {
		if !strings.HasSuffix(info.Name(), suffix) {
			continue
		}

		if content != nil {
			return nil, fmt.Errorf("more than one object in '%s'",
				path)
		}

		file, err = os.Open(path + "/" + info.Name())
		if err != nil {
			return nil, err
		}

		defer file.Close()

		content, err = ioutil.ReadAll(file)
		if err != nil {
			return nil, err
		}
	}

	return content, nil

}

type application struct {
	logger     core.Logger
	moduleCode []byte
	ctorCode   []byte
	funcCodes  map[string][]byte
	parser     *util.ServiceProcess
	scanner    *bufio.Scanner
	deployed   bool
}

type applicationArguments struct {
	funccode []byte
	funcargs []aptosmodels.TransactionArgument
}

func (a *application) arguments(function string, addr aptosmodels.AccountAddress) (*applicationArguments, error) {
	var ret applicationArguments
	var line string
	var arg []byte
	var err error
	var ok bool

	_, err = io.WriteString(a.parser, function+"\n")
	if err != nil {
		return nil, err
	}

	if !a.scanner.Scan() {
		return nil, fmt.Errorf("function '%s' parsing failed: scan",
			function)
	}

	line = a.scanner.Text()
	if line == "" {
		return nil, fmt.Errorf("function '%s' parsing failed: text",
			function)
	}

	ret.funccode, ok = a.funcCodes[line]
	if !ok {
		return nil, fmt.Errorf("unknown function name '%s'", line)
	}

	ret.funcargs = make([]aptosmodels.TransactionArgument, 0)
	for a.scanner.Scan() {
		line = a.scanner.Text()
		if line == "" {
			break
		}

		arg, err = base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, err
		}

		if len(arg) < 1 {
			return nil, fmt.Errorf("invalid argument format: '%s'",
				line)
		}

		switch arg[0] {
		case 'a':
			val := aptosmodels.TxArgAddress{Addr: addr}
			ret.funcargs = append(ret.funcargs, &val)
		case 'v':
			val := aptosmodels.TxArgU8Vector{Bytes: arg[1:]}
			ret.funcargs = append(ret.funcargs, &val)
		case '6':
			val := aptosmodels.TxArgU64{U64: binary.LittleEndian.Uint64(arg[1:])}
			ret.funcargs = append(ret.funcargs, &val)
		default:
			return nil, fmt.Errorf("unknown argument type '%c'",
				arg[0])
		}
	}

	return &ret, nil
}
