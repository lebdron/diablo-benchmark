package navalanche

import (
	"bufio"
	"bytes"
	"diablo-benchmark/core"
	"diablo-benchmark/util"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/compiler"
)

// Solidity contains information about the solidity compiler.
type Solidity struct {
	Path, Version, FullVersion string
	Major, Minor, Patch        int
}

func (s *Solidity) makeArgs() []string {
	p := []string{
		"--combined-json", "bin,bin-runtime,srcmap,srcmap-runtime,abi,userdoc,devdoc",
		"--optimize",                  // code optimizer switched on
		"--allow-paths", "., ./, ../", // default to support relative paths
	}
	if s.Major > 0 || s.Minor > 4 || s.Patch > 6 {
		p[1] += ",metadata,hashes"
	}
	return p
}

var versionRegexp = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

// SolidityVersion runs solc and parses its version output.
func SolidityVersion(solc string) (*Solidity, error) {
	if solc == "" {
		solc = "solc"
	}
	var out bytes.Buffer
	cmd := exec.Command(solc, "--version")
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	matches := versionRegexp.FindStringSubmatch(out.String())
	if len(matches) != 4 {
		return nil, fmt.Errorf("can't parse solc version %q", out.String())
	}
	s := &Solidity{Path: cmd.Path, FullVersion: out.String(), Version: matches[0]}
	if s.Major, err = strconv.Atoi(matches[1]); err != nil {
		return nil, err
	}
	if s.Minor, err = strconv.Atoi(matches[2]); err != nil {
		return nil, err
	}
	if s.Patch, err = strconv.Atoi(matches[3]); err != nil {
		return nil, err
	}
	return s, nil
}

// CompileSolidityString builds and returns all the contracts contained within a source string.
func CompileSolidityString(solc, source string) (map[string]*compiler.Contract, error) {
	if len(source) == 0 {
		return nil, errors.New("solc: empty source string")
	}
	s, err := SolidityVersion(solc)
	if err != nil {
		return nil, err
	}
	args := append(s.makeArgs(), "--")
	cmd := exec.Command(s.Path, append(args, "-")...)
	cmd.Stdin = strings.NewReader(source)
	return s.run(cmd, source)
}

func slurpFiles(files []string) (string, error) {
	var concat bytes.Buffer
	for _, file := range files {
		content, err := ioutil.ReadFile(file)
		if err != nil {
			return "", err
		}
		concat.Write(content)
	}
	return concat.String(), nil
}

// CompileSolidity compiles all given Solidity source files.
func CompileSolidity(solc string, sourcefiles ...string) (map[string]*compiler.Contract, error) {
	if len(sourcefiles) == 0 {
		return nil, errors.New("solc: no source files")
	}
	source, err := slurpFiles(sourcefiles)
	if err != nil {
		return nil, err
	}
	s, err := SolidityVersion(solc)
	if err != nil {
		return nil, err
	}
	args := append(s.makeArgs(), "--")
	cmd := exec.Command(s.Path, append(args, sourcefiles...)...)
	return s.run(cmd, source)
}

func (s *Solidity) run(cmd *exec.Cmd, source string) (map[string]*compiler.Contract, error) {
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("solc: %v\n%s", err, stderr.Bytes())
	}
	return compiler.ParseCombinedJSON(stdout.Bytes(), source, s.Version, s.Version, strings.Join(s.makeArgs(), " "))
}

type solidityCompiler struct {
	logger core.Logger
	base   string
}

func newSolidityCompiler(logger core.Logger, base string) *solidityCompiler {
	return &solidityCompiler{
		logger: logger,
		base:   base,
	}
}

func (this *solidityCompiler) compile(name string) (*application, error) {
	var contracts map[string]*compiler.Contract
	var contract *compiler.Contract
	var parser *util.ServiceProcess
	var entries map[string][]byte
	var path, fname, hash string
	var text []byte
	var err error

	this.logger.Debugf("compile contract '%s'", name)

	path = this.base + "/" + name + "/contract.sol"

	this.logger.Tracef("compile contract source in '%s'", path)

	contracts, err = CompileSolidity("", path)
	if err != nil {
		return nil, err
	} else if len(contracts) < 1 {
		return nil, fmt.Errorf("no contract in '%s'", path)
	} else if len(contracts) > 1 {
		return nil, fmt.Errorf("more than one contract in '%s'", path)
	}

	for _, contract = range contracts {
		break
	}

	text, err = hex.DecodeString(contract.Code[2:])
	if err != nil {
		return nil, err
	}

	entries = make(map[string][]byte)
	for fname, hash = range contract.Hashes {
		entries[fname], err = hex.DecodeString(hash)
		if err != nil {
			return nil, err
		}

		this.logger.Tracef("  has function %s", fname)
	}

	path = this.base + "/" + name + "/arguments"

	if !strings.HasPrefix(path, "/") {
		path = "./" + path
	}

	parser, err = util.StartServiceProcess(path)
	if err != nil {
		return nil, err
	}

	return newApplication(this.logger, text, entries, parser), nil
}

type application struct {
	logger  core.Logger
	text    []byte
	entries map[string][]byte
	parser  *util.ServiceProcess
	scanner *bufio.Scanner
}

func newApplication(logger core.Logger, text []byte, entries map[string][]byte, parser *util.ServiceProcess) *application {
	return &application{
		logger:  logger,
		text:    text,
		entries: entries,
		parser:  parser,
		scanner: bufio.NewScanner(parser),
	}
}

func (this *application) arguments(function string) ([]byte, error) {
	var entry, payload []byte
	var fname string
	var found bool
	var err error

	_, err = io.WriteString(this.parser, function+"\n")
	if err != nil {
		return nil, err
	}

	if this.scanner.Scan() {
		fname = this.scanner.Text()

		if this.scanner.Scan() {
			payload, err = base64.StdEncoding.
				DecodeString(this.scanner.Text())
			if err != nil {
				return nil, err
			}
		}
	}

	if payload == nil {
		err = this.scanner.Err()
		if err == nil {
			err = fmt.Errorf("EOF")
		}
		return nil, err
	}

	entry, found = this.entries[fname]
	if !found {
		return nil, fmt.Errorf("unknown function '%s'", fname)
	}

	return append(entry, payload...), nil
}
