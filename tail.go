// Copyright (c) 2015 HPE Software Inc. All rights reserved.
// Copyright (c) 2013 ActiveState Software Inc. All rights reserved.

package tail

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Pandry/tail/ratelimiter"
	"github.com/Pandry/tail/util"
	"github.com/Pandry/tail/watch"

	tomb "gopkg.in/tomb.v1"
)

var (
	ErrStop = errors.New("tail should now stop")
)

type Line struct {
	Text string
	Num  int
	Time time.Time
	Err  error // Error from tail
}

// NewLine returns a Line with present time.
func NewLine(text string, lineNum int) *Line {
	return &Line{text, lineNum, time.Now(), nil}
}

// SeekInfo represents arguments to `io.Seek`
type SeekInfo struct {
	Offset int64
	Whence int // io.Seek*
}

type logger interface {
	Fatal(v ...interface{})
	Fatalf(format string, v ...interface{})
	Fatalln(v ...interface{})
	Panic(v ...interface{})
	Panicf(format string, v ...interface{})
	Panicln(v ...interface{})
	Print(v ...interface{})
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// Config is used to specify how a file must be tailed.
type Config struct {
	// File-specifc
	Location    *SeekInfo // Seek to this location before tailing
	ReOpen      bool      // Reopen recreated files (tail -F)
	MustExist   bool      // Fail early if the file does not exist
	Poll        bool      // Poll for file changes instead of using inotify
	Pipe        bool      // Is a named pipe (mkfifo)
	RateLimiter *ratelimiter.LeakyBucket

	LastLines    int  // Output the last NUM lines (tail -n)
	FromLine     int  // Output starting with line NUM (tail -n)
	PageSize     int  // Buffer size for seek line. Default 4096
	SeekOnReOpen bool // Start from line on reopen

	// Generic IO
	Follow      bool // Continue looking for new lines (tail -f)
	MaxLineSize int  // If non-zero, split longer lines into multiple lines

	// Logger, when nil, is set to tail.DefaultLogger
	// To disable logging: set field to tail.DiscardingLogger
	Logger logger
}

type Tail struct {
	Filename string
	Lines    chan *Line
	Config

	file    *os.File
	reader  *bufio.Reader
	lineNum int

	watcher watch.FileWatcher
	changes *watch.FileChanges

	tomb.Tomb // provides: Done, Kill, Dying

	lk sync.Mutex
}

var (
	// DefaultLogger is used when Config.Logger == nil
	DefaultLogger = log.New(os.Stderr, "", log.LstdFlags)
	// DiscardingLogger can be used to disable logging output
	DiscardingLogger = log.New(ioutil.Discard, "", 0)
)

// TailFile begins tailing the file. Output stream is made available
// via the `Tail.Lines` channel. To handle errors during tailing,
// invoke the `Wait` or `Err` method after finishing reading from the
// `Lines` channel.
func TailFile(filename string, config Config) (*Tail, error) {
	if config.ReOpen && !config.Follow {
		util.Fatal("cannot set ReOpen without Follow.")
	}

	t := &Tail{
		Filename: filename,
		Lines:    make(chan *Line),
		Config:   config,
	}

	// when Logger was not specified in config, use default logger
	if t.Logger == nil {
		t.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	if t.Poll {
		t.watcher = watch.NewPollingFileWatcher(filename)
	} else {
		t.watcher = watch.NewInotifyFileWatcher(filename)
	}

	if t.MustExist {
		var err error
		t.file, err = OpenFile(t.Filename)
		if err != nil {
			return nil, err
		}
	}

	go t.tailFileSync()

	return t, nil
}

// Update tail.Config.Location corresponding to tail.Config.LastLines.
// It goes with step defined in PageSize or 4096 by default
// Or set to 0 if tail.Config.LastLines more that lines in file
func (tail *Tail) readLast() {

	count := 0
	nLines := tail.Config.LastLines

	seek := &SeekInfo{
		Offset: 0,
		Whence: 2, // io.SeekEnd
	}

	pos, err := tail.file.Seek(seek.Offset, seek.Whence)

	if err != nil && err != io.EOF {
		fmt.Printf("Seek error on %s: %s\n", tail.Filename, err)
	}

	pageSize := 4096
	if tail.Config.PageSize != 0 {
		pageSize = tail.Config.PageSize
	}

	// Skip сlosing \n if exist
	b1 := make([]byte, 1)
	tail.file.ReadAt(b1, pos-1)

	if '\n' == b1[0] {
		pos = pos - 1
	}

	for {
		readFrom := pos - int64(pageSize)
		lines := make([]int64, 0)

		if readFrom <= 0 {
			pageSize += int(readFrom)
			readFrom = 0
			lines = append(lines, 0)
			count++
		}

		b := make([]byte, pageSize)

		_, err := tail.file.ReadAt(b, readFrom)

		if err != nil && err != io.EOF {
			fmt.Printf("Read error on %s: %s\n", tail.Filename, err)
		}

		i := 0

		// Find newline symbols location in buffer and put it in to slice
		for {
			bufPos := bytes.IndexByte(b[i:], '\n')
			if bufPos == -1 {
				break
			}
			i = i + bufPos + 1

			lines = append(lines, int64(i))
			count++
		}

		var firstLinePos int64

		// If no lines found in buf set firstLinePos to 0
		// Its needed to handle lines bigger than PageSize
		if len(lines) == 0 {
			firstLinePos = 0
		} else {
			firstLinePos = lines[0]
		}

		if count == nLines {
			tail.Config.Location = &SeekInfo{
				Offset: firstLinePos + readFrom,
				Whence: 0, // io.SeekStart
			}
			return
		}

		if count > nLines {
			linesLeft := count - nLines
			targetPos := lines[linesLeft]
			tail.Config.Location = &SeekInfo{
				Offset: targetPos + readFrom,
				Whence: 0, // io.SeekStart
			}
			return
		}

		if readFrom == 0 {
			tail.Config.Location = &SeekInfo{
				Offset: 0,
				Whence: 0, // io.SeekStart
			}
			return
		}
		pos = firstLinePos + readFrom - 1
	}
}

// Update tail.Config.Location corresponding to tail.Config.FromLine
// Or set to end of the file if FromLine  more that lines in file
// it goes with step defined in PageSize or 4096 by default
func (tail *Tail) skipLines() {

	fileStat, err := tail.file.Stat()
	if err != nil {
		fmt.Println(err)
	}
	fileSize := fileStat.Size()

	count := 1
	nLines := tail.Config.FromLine

	pageSize := 4096
	if tail.Config.PageSize != 0 {
		pageSize = tail.Config.PageSize
	}

	// Skip сlosing \n if exist
	b1 := make([]byte, 1)
	tail.file.ReadAt(b1, fileSize-1)

	if '\n' == b1[0] {
		fileSize = fileSize - 1
	}

	var pos int64
	pos = 0

	for {

		b := make([]byte, pageSize)

		_, err := tail.file.ReadAt(b, pos)
		if err != nil && err != io.EOF {
			fmt.Printf("Read error on %s: %s\n", tail.Filename, err)
		}

		i := 0

		// Find newline symbols location in buffer and put it in to slice
		for {
			bufPos := bytes.IndexByte(b[i:], '\n')
			if bufPos == -1 {
				break
			}

			i = i + bufPos + 1

			count++
			if count == nLines {
				tail.Config.Location = &SeekInfo{
					Offset: int64(i) + pos,
					Whence: 0, // io.SeekStart
				}
				return
			}
		}

		pos = pos + int64(pageSize)

		if pos >= fileSize {
			tail.Config.Location = &SeekInfo{
				Offset: 0,
				Whence: 2, // io.SeekEnd
			}
			return
		}
	}
}

// Tell Return the file's current position, like stdio's ftell().
// But this value is not very accurate.
// it may readed one line in the chan(tail.Lines),
// so it may lost one line.
func (tail *Tail) Tell() (offset int64, err error) {
	if tail.file == nil {
		return
	}
	offset, err = tail.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	tail.lk.Lock()
	defer tail.lk.Unlock()
	if tail.reader == nil {
		return
	}

	offset -= int64(tail.reader.Buffered())
	return
}

// Stop stops the tailing activity.
func (tail *Tail) Stop() error {
	tail.Kill(nil)
	return tail.Wait()
}

// StopAtEOF stops tailing as soon as the end of the file is reached.
func (tail *Tail) StopAtEOF() error {
	tail.Kill(errStopAtEOF)
	return tail.Wait()
}

var errStopAtEOF = errors.New("tail: stop at eof")

func (tail *Tail) close() {
	close(tail.Lines)
	tail.closeFile()
}

func (tail *Tail) closeFile() {
	if tail.file != nil {
		tail.file.Close()
		tail.file = nil
	}
}

func (tail *Tail) reopen() error {
	tail.closeFile()
	// reset line number
	tail.lineNum = 0
	for {
		var err error
		tail.file, err = OpenFile(tail.Filename)
		if err != nil {
			if os.IsNotExist(err) {
				tail.Logger.Printf("Waiting for %s to appear...", tail.Filename)
				if err := tail.watcher.BlockUntilExists(&tail.Tomb); err != nil {
					if err == tomb.ErrDying {
						return err
					}
					return fmt.Errorf("Failed to detect creation of %s: %s", tail.Filename, err)
				}
				continue
			}
			return fmt.Errorf("Unable to open file %s: %s", tail.Filename, err)
		}
		break
	}
	return nil
}

func (tail *Tail) readLine() (string, error) {
	tail.lk.Lock()
	line, err := tail.reader.ReadString('\n')
	tail.lk.Unlock()
	if err != nil {
		// Note ReadString "returns the data read before the error" in
		// case of an error, including EOF, so we return it as is. The
		// caller is expected to process it if err is EOF.
		return line, err
	}

	line = strings.TrimRight(line, "\n")

	return line, err
}

func (tail *Tail) findLine() {

	if tail.Config.FromLine > 0 {
		tail.skipLines()
	} else if tail.Config.LastLines > 0 {
		tail.readLast()
	}
	// Seek to requested location on first open of the file.
	if tail.Location != nil {
		_, err := tail.file.Seek(tail.Location.Offset, tail.Location.Whence)
		//tail.Logger.Printf("Seeked %s - %+v\n", tail.Filename, tail.Location)
		if err != nil {
			tail.Killf("Seek error on %s: %s", tail.Filename, err)
			return
		}
	}
}

func (tail *Tail) tailFileSync() {
	defer tail.Done()
	defer tail.close()

	if !tail.MustExist {
		// deferred first open.
		err := tail.reopen()
		if err != nil {
			if err != tomb.ErrDying {
				tail.Kill(err)
			}
			return
		}
	}

	tail.findLine()
	tail.openReader()

	var offset int64
	var err error

	// Read line by line.
	for {
		// do not seek in named pipes
		if !tail.Pipe {
			// grab the position in case we need to back up in the event of a half-line
			offset, err = tail.Tell()
			if err != nil {
				tail.Kill(err)
				return
			}
		}

		line, err := tail.readLine()

		// Process `line` even if err is EOF.
		if err == nil {
			cooloff := !tail.sendLine(line)
			if cooloff {
				// Wait a second before seeking till the end of
				// file when rate limit is reached.
				msg := ("Too much log activity; waiting a second " +
					"before resuming tailing")
				tail.Lines <- &Line{msg, tail.lineNum, time.Now(), errors.New(msg)}
				select {
				case <-time.After(time.Second):
				case <-tail.Dying():
					return
				}
				if err := tail.seekEnd(); err != nil {
					tail.Kill(err)
					return
				}
			}
		} else if err == io.EOF {
			if !tail.Follow {
				if line != "" {
					tail.sendLine(line)
				}
				return
			}

			if tail.Follow && line != "" {
				// this has the potential to never return the last line if
				// it's not followed by a newline; seems a fair trade here
				err := tail.seekTo(SeekInfo{Offset: offset, Whence: 0})
				if err != nil {
					tail.Kill(err)
					return
				}
			}

			// When EOF is reached, wait for more data to become
			// available. Wait strategy is based on the `tail.watcher`
			// implementation (inotify or polling).
			err := tail.waitForChanges()
			if err != nil {
				if err != ErrStop {
					tail.Kill(err)
				}
				return
			}
		} else {
			// non-EOF error
			tail.Killf("Error reading %s: %s", tail.Filename, err)
			return
		}

		select {
		case <-tail.Dying():
			if tail.Err() == errStopAtEOF {
				continue
			}
			return
		default:
		}
	}
}

// waitForChanges waits until the file has been appended, deleted,
// moved or truncated. When moved or deleted - the file will be
// reopened if ReOpen is true. Truncated files are always reopened.
func (tail *Tail) waitForChanges() error {
	if tail.changes == nil {
		var pos int64
		var err error
		if !tail.Pipe {
			pos, err = tail.file.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
		}
		tail.changes, err = tail.watcher.ChangeEvents(&tail.Tomb, pos)
		if err != nil {
			return err
		}
	}

	select {
	case <-tail.changes.Modified:
		return nil
	case <-tail.changes.Deleted:
		tail.changes = nil
		if tail.ReOpen {
			// XXX: we must not log from a library.
			tail.Logger.Printf("Re-opening moved/deleted file %s ...", tail.Filename)
			if err := tail.reopen(); err != nil {
				return err
			}
			tail.Logger.Printf("Successfully reopened %s", tail.Filename)

			if tail.Config.SeekOnReOpen {
				tail.findLine()
			}
			tail.openReader()
			return nil
		} else {
			tail.Logger.Printf("Stopping tail as file no longer exists: %s", tail.Filename)
			return ErrStop
		}
	case <-tail.changes.Truncated:
		// Always reopen truncated files (Follow is true)
		tail.Logger.Printf("Re-opening truncated file %s ...", tail.Filename)
		if err := tail.reopen(); err != nil {
			return err
		}
		tail.Logger.Printf("Successfully reopened truncated %s", tail.Filename)
		tail.openReader()
		return nil
	case <-tail.Dying():
		return ErrStop
	}
	panic("unreachable")
}

func (tail *Tail) openReader() {
	if tail.MaxLineSize > 0 {
		// add 2 to account for newline characters
		tail.reader = bufio.NewReaderSize(tail.file, tail.MaxLineSize+2)
	} else {
		tail.reader = bufio.NewReader(tail.file)
	}
}

func (tail *Tail) seekEnd() error {
	if !tail.Pipe {
		return nil
	}
	return tail.seekTo(SeekInfo{Offset: 0, Whence: io.SeekEnd})
}

func (tail *Tail) seekTo(pos SeekInfo) error {
	_, err := tail.file.Seek(pos.Offset, pos.Whence)
	if err != nil {
		return fmt.Errorf("Seek error on %s: %s", tail.Filename, err)
	}
	// Reset the read buffer whenever the file is re-seek'ed
	tail.reader.Reset(tail.file)
	return nil
}

// sendLine sends the line(s) to Lines channel, splitting longer lines
// if necessary. Return false if rate limit is reached.
func (tail *Tail) sendLine(line string) bool {
	now := time.Now()
	lines := []string{line}

	// Split longer lines
	if tail.MaxLineSize > 0 && len(line) > tail.MaxLineSize {
		lines = util.PartitionString(line, tail.MaxLineSize)
	}

	for _, line := range lines {
		tail.lineNum++
		tail.Lines <- &Line{line, tail.lineNum, now, nil}
	}

	if tail.Config.RateLimiter != nil {
		ok := tail.Config.RateLimiter.Pour(uint16(len(lines)))
		if !ok {
			tail.Logger.Printf("Leaky bucket full (%v); entering 1s cooloff period.\n",
				tail.Filename)
			return false
		}
	}

	return true
}

// Cleanup removes inotify watches added by the tail package. This function is
// meant to be invoked from a process's exit handler. Linux kernel may not
// automatically remove inotify watches after the process exits.
func (tail *Tail) Cleanup() {
	watch.Cleanup(tail.Filename)
}
