package checklog

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/mackerelio/checkers"
	"github.com/mackerelio/mackerel-agent/logging"
	"github.com/mattn/go-encoding"
	"github.com/mattn/go-zglob"
	enc "golang.org/x/text/encoding"
)

var logger = logging.GetLogger(fmt.Sprintf("check.plugin.log.Pid%d", os.Getpid()))

type logOpts struct {
	LogFile             string  `short:"f" long:"file" value-name:"FILE" description:"Path to log file"`
	Pattern             string  `short:"p" long:"pattern" required:"true" value-name:"PAT" description:"Pattern to search for"`
	Exclude             string  `short:"E" long:"exclude" value-name:"PAT" description:"Pattern to exclude from matching"`
	WarnOver            int64   `short:"w" long:"warning-over" description:"Trigger a warning if matched lines is over a number"`
	CritOver            int64   `short:"c" long:"critical-over" description:"Trigger a critical if matched lines is over a number"`
	WarnLevel           float64 `long:"warning-level" value-name:"N" description:"Warning level if pattern has a group"`
	CritLevel           float64 `long:"critical-level" value-name:"N" description:"Critical level if pattern has a group"`
	ReturnContent       bool    `short:"r" long:"return" description:"Return matched line"`
	FilePattern         string  `short:"F" long:"file-pattern" value-name:"FILE" description:"Check a pattern of files, instead of one file"`
	CaseInsensitive     bool    `short:"i" long:"icase" description:"Run a case insensitive match"`
	StateDir            string  `short:"s" long:"state-dir" value-name:"DIR" description:"Dir to keep state files under"`
	NoState             bool    `long:"no-state" description:"Don't use state file and read whole logs"`
	Encoding            string  `long:"encoding" description:"Encoding of log file"`
	Missing             string  `long:"missing" default:"UNKNOWN" value-name:"(CRITICAL|WARNING|OK|UNKNOWN)" description:"Exit status when log files missing"`
	Debug               bool    `long:"debug" description:"Outputs some debug logs to STDERR"`
	patternReg          *regexp.Regexp
	excludeReg          *regexp.Regexp
	fileListFromGlob    []string
	fileListFromPattern []string
	origArgs            []string
	decoder             *enc.Decoder
}

func (opts *logOpts) prepare() error {
	if opts.LogFile == "" && opts.FilePattern == "" {
		return fmt.Errorf("No log file specified")
	}

	if opts.Debug {
		logging.SetLogLevel(logging.DEBUG)
	} else {
		logging.SetLogLevel(logging.ERROR)
	}

	var err error
	if opts.patternReg, err = regCompileWithCase(opts.Pattern, opts.CaseInsensitive); err != nil {
		return fmt.Errorf("pattern is invalid")
	}

	if opts.Exclude != "" {
		opts.excludeReg, err = regCompileWithCase(opts.Exclude, opts.CaseInsensitive)
		if err != nil {
			return fmt.Errorf("exclude pattern is invalid")
		}
	}

	if opts.LogFile != "" {
		opts.fileListFromGlob, err = zglob.Glob(opts.LogFile)
		// unless --missing specified, we should ignore file not found error
		if err != nil && err != os.ErrNotExist {
			return fmt.Errorf("invalid glob for --file")
		}
		logger.Debugf("check against files %s from glob %s", opts.fileListFromGlob, opts.LogFile)
	}

	if opts.FilePattern != "" {
		dirStr := filepath.Dir(opts.FilePattern)
		filePat := filepath.Base(opts.FilePattern)
		reg, err := regCompileWithCase(filePat, opts.CaseInsensitive)
		if err != nil {
			return fmt.Errorf("file-pattern is invalid")
		}

		fileInfos, err := ioutil.ReadDir(dirStr)
		if err != nil {
			return fmt.Errorf("cannot read the directory:" + err.Error())
		}

		for _, fileInfo := range fileInfos {
			if fileInfo.IsDir() {
				continue
			}
			fname := fileInfo.Name()
			if reg.MatchString(fname) {
				opts.fileListFromPattern = append(opts.fileListFromPattern, dirStr+string(filepath.Separator)+fileInfo.Name())
			}
		}
	}
	if !validateMissing(opts.Missing) {
		return fmt.Errorf("missing option is invalid")
	}
	return nil
}

// Do the plugin
func Do() {
	ckr := runWithTimeout(os.Args[1:])
	ckr.Name = "LOG"
	ckr.Exit()
}

func regCompileWithCase(ptn string, caseInsensitive bool) (*regexp.Regexp, error) {
	if caseInsensitive {
		ptn = "(?i)" + ptn
	}
	return regexp.Compile(ptn)
}

func validateMissing(missing string) bool {
	switch missing {
	case "CRITICAL", "WARNING", "OK", "UNKNOWN", "":
		return true
	default:
		return false
	}
}

func parseArgs(args []string) (*logOpts, error) {
	origArgs := make([]string, len(args))
	copy(origArgs, args)
	opts := &logOpts{}
	_, err := flags.ParseArgs(opts, args)
	opts.origArgs = origArgs
	if opts.StateDir == "" {
		workdir := os.Getenv("MACKEREL_PLUGIN_WORKDIR")
		if workdir == "" {
			workdir = os.TempDir()
		}
		opts.StateDir = filepath.Join(workdir, "check-log")
	}
	return opts, err
}

func runWithTimeout(args []string) *checkers.Checker {
	done := make(chan *checkers.Checker, 1)
	go func(args []string) {
		done <- run(args)
	}(args)

	select {
	case chr := <-done:
		return chr
	case <-time.After(time.Second * 120):
		// timeout
	}
	return checkers.Unknown("timed out")
}

func run(args []string) *checkers.Checker {
	opts, err := parseArgs(args)
	if err != nil {
		os.Exit(1)
	}

	err = opts.prepare()
	if err != nil {
		return checkers.Unknown(err.Error())
	}

	warnNum := int64(0)
	critNum := int64(0)
	var missingFiles []string
	errorOverall := ""

	if opts.LogFile != "" && len(opts.fileListFromGlob) == 0 {
		missingFiles = append(missingFiles, opts.LogFile)
	}

	for _, f := range append(opts.fileListFromGlob, opts.fileListFromPattern...) {
		logger.Debugf("check against file %s", f)
		_, err := os.Stat(f)
		if err != nil {
			missingFiles = append(missingFiles, f)
			continue
		}
		w, c, errLines, err := opts.searchLog(f)
		if err != nil {
			return checkers.Unknown(err.Error())
		}
		logger.Debugf("check result for file %s w: %d, c: %d", f, w, c)
		warnNum += w
		critNum += c
		if opts.ReturnContent {
			errorOverall += errLines
		}
	}

	msg := fmt.Sprintf("%d warnings, %d criticals for pattern /%s/.", warnNum, critNum, opts.Pattern)
	if errorOverall != "" {
		msg += "\n" + errorOverall
	}
	checkSt := checkers.OK
	if len(missingFiles) > 0 {
		switch opts.Missing {
		case "OK":
		case "WARNING":
			checkSt = checkers.WARNING
		case "CRITICAL":
			checkSt = checkers.CRITICAL
		default:
			checkSt = checkers.UNKNOWN
		}
		msg += "\n" + fmt.Sprintf("The following %d files are missing.", len(missingFiles))
		for _, f := range missingFiles {
			msg += "\n" + f
		}
	}
	if warnNum > opts.WarnOver {
		checkSt = checkers.WARNING
	}
	if critNum > opts.CritOver {
		checkSt = checkers.CRITICAL
	}
	return checkers.NewChecker(checkSt, msg)
}

func (opts *logOpts) searchLog(logFile string) (int64, int64, string, error) {
	stateFile := getStateFile(opts.StateDir, logFile, opts.origArgs)
	skipBytes := int64(0)
	if !opts.NoState {
		s, err := getBytesToSkip(stateFile)
		if err != nil {
			return 0, 0, "", err
		}
		skipBytes = s
		logger.Debugf("skipping %d bytes", skipBytes)
	}

	logger.Debugf("Opening file %s", logFile)
	f, err := os.Open(logFile)
	if err != nil {
		return 0, 0, "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return 0, 0, "", err
	}

	rotated := false
	if stat.Size() < skipBytes {
		logger.Debugf("Seems rotated")
		rotated = true
	} else if skipBytes > 0 {
		f.Seek(skipBytes, 0)
	}

	var r io.Reader = f
	if opts.Encoding != "" {
		e := encoding.GetEncoding(opts.Encoding)
		if e == nil {
			return 0, 0, "", fmt.Errorf("unknown encoding:" + opts.Encoding)
		}
		opts.decoder = e.NewDecoder()
	}

	warnNum, critNum, readBytes, errLines, err := opts.searchReader(r)
	if err != nil {
		return warnNum, critNum, errLines, err
	}

	if rotated {
		skipBytes = readBytes
	} else {
		skipBytes += readBytes
	}

	if !opts.NoState {
		logger.Debugf("Write to stateFile %s", stateFile)
		err = writeBytesToSkip(stateFile, skipBytes)
		if err != nil {
			log.Printf("writeByteToSkip failed: %s\n", err.Error())
		}
	}
	return warnNum, critNum, errLines, nil
}

func (opts *logOpts) searchReader(rdr io.Reader) (warnNum, critNum, readBytes int64, errLines string, err error) {
	r := bufio.NewReader(rdr)
	i := 0
	for {
		i++
		lineBytes, rErr := r.ReadBytes('\n')
		if rErr != nil {
			if rErr != io.EOF {
				err = rErr
			}
			break
		}
		if i%100 == 1 {
			logger.Debugf("Read %d bytes for %d th line", len(lineBytes), i)
		}
		readBytes += int64(len(lineBytes))

		if opts.decoder != nil {
			lineBytes, err = opts.decoder.Bytes(lineBytes)
			if err != nil {
				break
			}
		}
		line := strings.Trim(string(lineBytes), "\r\n")
		if matched, matches := opts.match(line); matched {
			if len(matches) > 1 && (opts.WarnLevel > 0 || opts.CritLevel > 0) {
				level, err := strconv.ParseFloat(matches[1], 64)
				if err != nil {
					warnNum++
					critNum++
					errLines += line + "\n"
				} else {
					levelOver := false
					if level > opts.WarnLevel {
						levelOver = true
						warnNum++
					}
					if level > opts.CritLevel {
						levelOver = true
						critNum++
					}
					if levelOver {
						errLines += line + "\n"
					}
				}
			} else {
				warnNum++
				critNum++
				errLines += line + "\n"
			}
		}
	}
	return
}

func (opts *logOpts) match(line string) (bool, []string) {
	pReg := opts.patternReg
	eReg := opts.excludeReg

	matches := pReg.FindStringSubmatch(line)
	matched := len(matches) > 0 && (eReg == nil || !eReg.MatchString(line))
	return matched, matches
}

var stateRe = regexp.MustCompile(`^([A-Z]):[/\\]`)

func getStateFile(stateDir, f string, args []string) string {
	return filepath.Join(
		stateDir,
		fmt.Sprintf(
			"%s-%x",
			stateRe.ReplaceAllString(f, `$1`+string(filepath.Separator)),
			md5.Sum([]byte(strings.Join(args, " "))),
		),
	)
}

func getBytesToSkip(f string) (int64, error) {
	_, err := os.Stat(f)
	if err != nil {
		return 0, nil
	}
	b, err := ioutil.ReadFile(f)
	if err != nil {
		return 0, err
	}
	i, err := strconv.ParseInt(strings.Trim(string(b), " \r\n"), 10, 64)
	if err != nil {
		log.Printf("failed to getBytesToSkip (ignoring): %s", err)
	}
	return i, nil
}

func writeBytesToSkip(f string, num int64) error {
	err := os.MkdirAll(filepath.Dir(f), 0755)
	if err != nil {
		return err
	}
	return writeFileAtomically(f, []byte(fmt.Sprintf("%d", num)))
}

func writeFileAtomically(f string, contents []byte) error {
	// MUST be located on same disk partition
	tmpf, err := ioutil.TempFile(filepath.Dir(f), "tmp")
	if err != nil {
		return err
	}
	// os.Remove here works successfully when tmpf.Write fails or os.Rename fails.
	// In successful case, os.Remove fails because the temporary file is already renamed.
	defer os.Remove(tmpf.Name())
	_, err = tmpf.Write(contents)
	tmpf.Close() // should be called before rename
	if err != nil {
		return err
	}
	return os.Rename(tmpf.Name(), f)
}
