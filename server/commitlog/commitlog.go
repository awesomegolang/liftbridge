// Package commitlog provides an implementation for a file-backed write-ahead
// log.
package commitlog

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	atomic_file "github.com/natefinch/atomic"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/liftbridge-io/liftbridge/server/logger"
	"github.com/liftbridge-io/liftbridge/server/proto"
)

var (
	ErrSegmentNotFound = errors.New("segment not found")
)

const (
	logFileSuffix               = ".log"
	indexFileSuffix             = ".index"
	hwFileName                  = "replication-offset-checkpoint"
	defaultMaxSegmentBytes      = 1073741824
	defaultHWCheckpointInterval = 5 * time.Second
	defaultCleanerInterval      = 5 * time.Minute
)

// CommitLog implements the server.CommitLog interface, which is a durable
// write-ahead log.
type CommitLog struct {
	Options
	cleaner        Cleaner
	name           string
	mu             sync.RWMutex
	hw             int64
	closed         chan struct{}
	segments       []*Segment
	vActiveSegment *Segment
	hwWaiters      map[io.Reader]chan struct{}
}

// Options contains settings for configuring a CommitLog.
type Options struct {
	Path                 string        // Path to log directory
	MaxSegmentBytes      int64         // Max number of bytes a Segment can contain before creating a new Segment
	MaxLogBytes          int64         // Retention by bytes
	MaxLogMessages       int64         // Retention by messages
	MaxLogAge            time.Duration // Retention by age
	CleanerInterval      time.Duration // Frequency to enforce retention policy
	HWCheckpointInterval time.Duration // Frequency to checkpoint HW to disk
	LogRollTime          time.Duration // Max time before a new log segment is rolled out.
	Logger               logger.Logger
}

// New creates a new CommitLog and starts a background goroutine which
// periodically checkpoints the high watermark to disk.
func New(opts Options) (*CommitLog, error) {
	if opts.Path == "" {
		return nil, errors.New("path is empty")
	}

	if opts.Logger == nil {
		opts.Logger = &log.Logger{Out: ioutil.Discard}
	}

	if opts.MaxSegmentBytes == 0 {
		opts.MaxSegmentBytes = defaultMaxSegmentBytes
	}
	if opts.HWCheckpointInterval == 0 {
		opts.HWCheckpointInterval = defaultHWCheckpointInterval
	}
	if opts.CleanerInterval == 0 {
		opts.CleanerInterval = defaultCleanerInterval
	}

	cleanerOpts := DeleteCleanerOptions{
		Name:   opts.Path,
		Logger: opts.Logger,
	}
	cleanerOpts.Retention.Bytes = opts.MaxLogBytes
	cleanerOpts.Retention.Messages = opts.MaxLogMessages
	cleanerOpts.Retention.Age = opts.MaxLogAge
	cleaner := NewDeleteCleaner(cleanerOpts)

	path, _ := filepath.Abs(opts.Path)
	l := &CommitLog{
		Options:   opts,
		name:      filepath.Base(path),
		cleaner:   cleaner,
		hw:        -1,
		closed:    make(chan struct{}),
		hwWaiters: make(map[io.Reader]chan struct{}),
	}

	if err := l.init(); err != nil {
		return nil, err
	}

	if err := l.open(); err != nil {
		return nil, err
	}

	go l.checkpointHWLoop()
	go l.cleanerLoop()

	return l, nil
}

func (l *CommitLog) init() error {
	err := os.MkdirAll(l.Path, 0755)
	if err != nil {
		return errors.Wrap(err, "mkdir failed")
	}
	return nil
}

func (l *CommitLog) open() error {
	files, err := ioutil.ReadDir(l.Path)
	if err != nil {
		return errors.Wrap(err, "read dir failed")
	}
	for _, file := range files {
		// if this file is an index file, make sure it has a corresponding .log file
		if strings.HasSuffix(file.Name(), indexFileSuffix) {
			_, err := os.Stat(filepath.Join(
				l.Path, strings.Replace(file.Name(), indexFileSuffix, logFileSuffix, 1)))
			if os.IsNotExist(err) {
				if err := os.Remove(file.Name()); err != nil {
					return err
				}
			} else if err != nil {
				return errors.Wrap(err, "stat file failed")
			}
		} else if strings.HasSuffix(file.Name(), logFileSuffix) {
			offsetStr := strings.TrimSuffix(file.Name(), logFileSuffix)
			baseOffset, err := strconv.Atoi(offsetStr)
			if err != nil {
				return err
			}
			segment, err := NewSegment(l.Path, int64(baseOffset), l.MaxSegmentBytes, false)
			if err != nil {
				return err
			}
			l.segments = append(l.segments, segment)
		} else if file.Name() == hwFileName {
			// Recover high watermark.
			b, err := ioutil.ReadFile(filepath.Join(l.Path, file.Name()))
			if err != nil {
				return errors.Wrap(err, "read high watermark file failed")
			}
			hw, err := strconv.ParseInt(string(b), 10, 64)
			if err != nil {
				return errors.Wrap(err, "parse high watermark file failed")
			}
			l.hw = hw
		}
	}
	if len(l.segments) == 0 {
		segment, err := NewSegment(l.Path, 0, l.MaxSegmentBytes, true)
		if err != nil {
			return err
		}
		l.segments = append(l.segments, segment)
	}
	activeSegment := l.segments[len(l.segments)-1]
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&l.vActiveSegment)),
		unsafe.Pointer(activeSegment))
	return nil
}

// Append writes the given batch of messages to the log and returns their
// corresponding offsets in the log.
func (l *CommitLog) Append(msgs []*proto.Message) ([]int64, error) {
	if _, err := l.checkAndPerformSplit(); err != nil {
		return nil, err
	}
	var (
		segment          = l.activeSegment()
		basePosition     = segment.Position()
		baseOffset       = segment.NextOffset()
		ms, entries, err = NewMessageSetFromProto(baseOffset, basePosition, msgs)
	)
	if err != nil {
		return nil, err
	}
	return l.append(segment, ms, entries)
}

// AppendMessageSet writes the given message set data to the log and returns
// the corresponding offsets in the log.
func (l *CommitLog) AppendMessageSet(ms []byte) ([]int64, error) {
	if _, err := l.checkAndPerformSplit(); err != nil {
		return nil, err
	}
	var (
		segment      = l.activeSegment()
		basePosition = segment.Position()
		baseOffset   = segment.NextOffset()
		entries      = EntriesForMessageSet(baseOffset, basePosition, ms)
	)
	return l.append(segment, ms, entries)
}

func (l *CommitLog) append(segment *Segment, ms []byte, entries []*Entry) ([]int64, error) {
	if err := segment.WriteMessageSet(ms, entries); err != nil {
		return nil, err
	}
	offsets := make([]int64, len(entries))
	for i, entry := range entries {
		offsets[i] = entry.Offset
	}
	return offsets, nil
}

// NewestOffset returns the offset of the last message in the log.
func (l *CommitLog) NewestOffset() int64 {
	return l.activeSegment().NextOffset() - 1
}

// OldestOffset returns the offset of the first message in the log.
func (l *CommitLog) OldestOffset() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[0].BaseOffset
}

// OffsetForTimestamp returns the earliest offset whose timestamp is greater
// than or equal to the given timestamp.
func (l *CommitLog) OffsetForTimestamp(timestamp int64) (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Find the first segment whose base timestamp is greater than the given
	// timestamp.
	idx, err := findSegmentIndexByTimestamp(l.segments, timestamp)
	if err != nil {
		return 0, errors.Wrap(err, "failed to find log segment for timestamp")
	}
	// Search the previous segment for the first entry whose timestamp is
	// greater than or equal to the given timestamp. If this is the first
	// segment, just search it.
	var seg *Segment
	if idx == 0 {
		seg = l.segments[0]
	} else {
		seg = l.segments[idx-1]
	}
	entry, err := seg.findEntryByTimestamp(timestamp)
	if err == nil {
		return entry.Offset, nil
	}
	if err != ErrEntryNotFound && err != io.EOF {
		return 0, errors.Wrap(err, "failed to find log entry for timestamp")
	}
	// This indicates there are no entries in the segment whose timestamp
	// is greater than or equal to the target timestamp. In this case, search
	// the next segment if there is one. If there isn't, the timestamp is
	// beyond the end of the log so return the next offset.
	if idx < len(l.segments) {
		seg = l.segments[idx]
		entry, err := seg.findEntryByTimestamp(timestamp)
		if err != nil {
			return 0, errors.Wrap(err, "failed to find log entry for timestamp")
		}
		return entry.Offset, nil
	}
	return l.segments[len(l.segments)-1].NextOffset(), nil
}

// SetHighWatermark sets the high watermark on the log. All messages up to and
// including the high watermark are considered committed.
func (l *CommitLog) SetHighWatermark(hw int64) {
	l.mu.Lock()
	if hw > l.hw {
		l.hw = hw
		l.notifyHWWaiters()
	}
	l.mu.Unlock()
	// TODO: should we flush the HW to disk here?
}

func (l *CommitLog) notifyHWWaiters() {
	for r, ch := range l.hwWaiters {
		close(ch)
		delete(l.hwWaiters, r)
	}
}

func (c *CommitLog) waitForHW(r io.Reader, hw int64) <-chan struct{} {
	wait := make(chan struct{})
	c.mu.Lock()
	// Check if HW has changed.
	if c.hw != hw {
		close(wait)
	} else {
		c.hwWaiters[r] = wait
	}
	c.mu.Unlock()
	return wait
}

func (c *CommitLog) removeHWWaiter(r io.Reader) {
	c.mu.Lock()
	delete(c.hwWaiters, r)
	c.mu.Unlock()
}

// HighWatermark returns the high watermark for the log.
func (l *CommitLog) HighWatermark() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hw
}

func (l *CommitLog) activeSegment() *Segment {
	return (*Segment)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&l.vActiveSegment))))
}

// Close closes each log segment file and stops the background goroutine
// checkpointing the high watermark to disk.
func (l *CommitLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkpointHW(); err != nil {
		return err
	}
	close(l.closed)
	for _, segment := range l.segments {
		if err := segment.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Delete closes the log and removes all data associated with it from the
// filesystem.
func (l *CommitLog) Delete() error {
	if err := l.Close(); err != nil {
		return err
	}
	return os.RemoveAll(l.Path)
}

// Truncate removes all messages from the log starting at the given offset.
func (l *CommitLog) Truncate(offset int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	segment, idx := findSegment(l.segments, offset)
	if segment == nil {
		// Nothing to truncate.
		return nil
	}

	// Delete all following segments.
	deleted := 0
	for i := idx + 1; i < len(l.segments); i++ {
		if err := l.segments[i].Delete(); err != nil {
			return err
		}
		deleted++
	}

	var replace bool

	// Delete the segment if its base offset is the target offset, provided
	// it's not the first segment.
	if segment.BaseOffset == offset {
		if idx == 0 {
			replace = true
		} else {
			if err := segment.Delete(); err != nil {
				return err
			}
			deleted++
		}
	} else {
		replace = true
	}

	// Retain all preceding segments.
	segments := make([]*Segment, len(l.segments)-deleted)
	for i := 0; i < idx; i++ {
		segments[i] = l.segments[i]
	}

	// Replace segment containing offset with truncated segment.
	if replace {
		var (
			ss              = NewSegmentScanner(segment)
			newSegment, err = NewSegment(
				segment.path, segment.BaseOffset,
				segment.maxBytes, true, truncatedSuffix)
		)
		if err != nil {
			return err
		}
		for ms, entry, err := ss.Scan(); err == nil; ms, entry, err = ss.Scan() {
			if ms.Offset() < offset {
				if err := newSegment.WriteMessageSet(ms, []*Entry{entry}); err != nil {
					return err
				}
			} else {
				break
			}
		}
		if err = newSegment.Replace(segment); err != nil {
			return err
		}
		segments[idx] = newSegment
	}
	activeSegment := segments[len(segments)-1]
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&l.vActiveSegment)),
		unsafe.Pointer(activeSegment))
	l.segments = segments
	return nil
}

func (l *CommitLog) Segments() []*Segment {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments
}

// checkAndPerformSplit determines if a new log segment should be rolled out
// either because the active segment is full or LogRollTime has passed since
// the first message was written to it. It then performs the split if eligible,
// returning any error resulting from the split. The returned bool indicates if
// a split was performed.
func (l *CommitLog) checkAndPerformSplit() (split bool, err error) {
	// Do this in a loop because segment splitting may fail due to a competing
	// thread performing the split at the same time. If this happens, we just
	// retry the check on the new active segment.
	for {
		activeSegment := l.activeSegment()
		if !activeSegment.CheckSplit(l.LogRollTime) {
			return
		}
		split = true
		if err := l.split(activeSegment); err != nil {
			// ErrSegmentExists indicates another thread has already performed
			// the segment split, so reload the new active segment and check
			// again.
			if err == ErrSegmentExists {
				continue
			}
			return false, err
		}
		activeSegment.Seal()
	}
}

func (l *CommitLog) split(oldActiveSegment *Segment) error {
	// TODO: We should shrink the previous active segment's index after rolling
	// the new segment.
	offset := l.NewestOffset() + 1
	l.Logger.Debugf("Appending new log segment for %s with base offset %d", l.Path, offset)
	segment, err := NewSegment(l.Path, offset, l.MaxSegmentBytes, true)
	if err != nil {
		return err
	}
	// Do a CAS on the active segment to ensure no other threads have replaced
	// it already. If this fails, it means another thread has already replaced
	// it, so delete the new segment and return ErrSegmentExists.
	if !atomic.CompareAndSwapPointer(
		(*unsafe.Pointer)(unsafe.Pointer(&l.vActiveSegment)),
		unsafe.Pointer(oldActiveSegment), unsafe.Pointer(segment)) {
		segment.Delete()
		return ErrSegmentExists
	}
	l.mu.Lock()
	l.segments = append(l.segments, segment)
	segments, err := l.cleaner.Clean(l.segments)
	if err != nil {
		l.mu.Unlock()
		return err
	}
	l.segments = segments
	l.mu.Unlock()
	return nil
}

func (l *CommitLog) cleanerLoop() {
	ticker := time.NewTicker(l.CleanerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-l.closed:
			return
		}

		// Check to see if the active segment should be split.
		split, err := l.checkAndPerformSplit()
		if err != nil {
			l.Logger.Errorf("Failed to split log %s: %v", l.Path, err)
			continue
		}

		// If we rolled a new segment, we don't need to run the cleaner since
		// it already ran.
		if split {
			continue
		}

		l.mu.Lock()
		segments, err := l.cleaner.Clean(l.segments)
		if err != nil {
			l.Logger.Errorf("Failed to clean log %s: %v", l.Path, err)
		} else {
			l.segments = segments
		}
		l.mu.Unlock()
	}
}

func (l *CommitLog) checkpointHWLoop() {
	ticker := time.NewTicker(l.HWCheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-l.closed:
			return
		}
		l.mu.RLock()
		if err := l.checkpointHW(); err != nil {
			panic(errors.Wrap(err, "failed to checkpoint high watermark"))
		}
		l.mu.RUnlock()
	}
}

func (l *CommitLog) checkpointHW() error {
	var (
		hw   = l.hw
		r    = strings.NewReader(strconv.FormatInt(hw, 10))
		file = filepath.Join(l.Path, hwFileName)
	)
	return atomic_file.WriteFile(file, r)
}
