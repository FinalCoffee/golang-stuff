package raft

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

//------------------------------------------------------------------------------
//
// Typedefs
//
//------------------------------------------------------------------------------

// A log is a collection of log entries that are persisted to durable storage.
type Log struct {
	ApplyFunc   func(Command) (interface{}, error)
	file        *os.File
	path        string
	entries     []*LogEntry
	results     []*logResult
	commitIndex uint64
	mutex       sync.RWMutex
	startIndex  uint64 // the index before the first entry in the Log entries
	startTerm   uint64
}

// The results of the applying a log entry.
type logResult struct {
	returnValue interface{}
	err         error
}

//------------------------------------------------------------------------------
//
// Constructor
//
//------------------------------------------------------------------------------

// Creates a new log.
func newLog() *Log {
	return &Log{
		entries: make([]*LogEntry, 0),
	}
}

//------------------------------------------------------------------------------
//
// Accessors
//
//------------------------------------------------------------------------------

//--------------------------------------
// Log Indices
//--------------------------------------

// The last committed index in the log.
func (l *Log) CommitIndex() uint64 {
	l.mutex.RLock()
	defer l.mutex.RUnlock()
	return l.commitIndex
}

// The current index in the log.
func (l *Log) currentIndex() uint64 {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	if len(l.entries) == 0 {
		return l.startIndex
	}
	return l.entries[len(l.entries)-1].Index
}

// The current index in the log without locking
func (l *Log) internalCurrentIndex() uint64 {
	if len(l.entries) == 0 {
		return l.startIndex
	}
	return l.entries[len(l.entries)-1].Index
}

// The next index in the log.
func (l *Log) nextIndex() uint64 {
	return l.currentIndex() + 1
}

// Determines if the log contains zero entries.
func (l *Log) isEmpty() bool {
	l.mutex.RLock()
	defer l.mutex.RUnlock()
	return (len(l.entries) == 0) && (l.startIndex == 0)
}

// The name of the last command in the log.
func (l *Log) lastCommandName() string {
	l.mutex.RLock()
	defer l.mutex.RUnlock()
	if len(l.entries) > 0 {
		if command := l.entries[len(l.entries)-1].Command; command != nil {
			return command.CommandName()
		}
	}
	return ""
}

//--------------------------------------
// Log Terms
//--------------------------------------

// The current term in the log.
func (l *Log) currentTerm() uint64 {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	if len(l.entries) == 0 {
		return l.startTerm
	}
	return l.entries[len(l.entries)-1].Term
}

//------------------------------------------------------------------------------
//
// Methods
//
//------------------------------------------------------------------------------

//--------------------------------------
// State
//--------------------------------------

// Opens the log file and reads existing entries. The log can remain open and
// continue to append entries to the end of the log.
func (l *Log) open(path string) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	// Read all the entries from the log if one exists.
	var lastIndex int = 0
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// Open the log file.
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		reader := bufio.NewReader(file)

		// Read the file and decode entries.
		for {
			if _, err := reader.Peek(1); err == io.EOF {
				break
			}

			// Instantiate log entry and decode into it.
			entry := newLogEntry(l, 0, 0, nil)
			n, err := entry.decode(reader)
			if err != nil {
				file.Close()
				if err = os.Truncate(path, int64(lastIndex)); err != nil {
					return fmt.Errorf("raft.Log: Unable to recover: %v", err)
				}
				break
			}

			// Append entry.
			l.entries = append(l.entries, entry)
			l.commitIndex = entry.Index

			// Apply the command.
			returnValue, err := l.ApplyFunc(entry.Command)
			l.results = append(l.results, &logResult{returnValue: returnValue, err: err})

			lastIndex += n
		}

		file.Close()
	}

	// Open the file for appending.
	var err error
	l.file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	l.path = path
	return nil
}

// Closes the log file.
func (l *Log) close() {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	l.entries = make([]*LogEntry, 0)
	l.results = make([]*logResult, 0)
}

//--------------------------------------
// Entries
//--------------------------------------

// Creates a log entry associated with this log.
func (l *Log) createEntry(term uint64, command Command) *LogEntry {
	return newLogEntry(l, l.nextIndex(), term, command)
}

// Retrieves an entry from the log. If the entry has been eliminated because
// of a snapshot then nil is returned.
func (l *Log) getEntry(index uint64) *LogEntry {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if index <= l.startIndex || index > (l.startIndex+uint64(len(l.entries))) {
		return nil
	}
	return l.entries[index-1]
}

// Checks if the log contains a given index/term combination.
func (l *Log) containsEntry(index uint64, term uint64) bool {
	entry := l.getEntry(index)
	return (entry != nil && entry.Term == term)
}

// Retrieves a list of entries after a given index as well as the term of the
// index provided. A nil list of entries is returned if the index no longer
// exists because a snapshot was made.
func (l *Log) getEntriesAfter(index uint64) ([]*LogEntry, uint64) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	// Return nil if index is before the start of the log.
	if index < l.startIndex {
		traceln("log.entriesAfter.before: ", index, " ", l.startIndex)
		return nil, 0
	}

	// Return an error if the index doesn't exist.
	if index > (uint64(len(l.entries)) + l.startIndex) {
		panic(fmt.Sprintf("raft: Index is beyond end of log: %v %v", len(l.entries), index))
	}

	// If we're going from the beginning of the log then return the whole log.
	if index == l.startIndex {
		traceln("log.entriesAfter.beginning: ", index, " ", l.startIndex)
		return l.entries, l.startTerm
	}

	traceln("log.entriesAfter.partial: ", index, " ", l.entries[len(l.entries)-1].Index)

	// Determine the term at the given entry and return a subslice.
	return l.entries[index-l.startIndex:], l.entries[index-1-l.startIndex].Term
}

// Retrieves the return value and error for an entry. The result can only exist
// after the entry has been committed.
func (l *Log) getEntryResult(entry *LogEntry, clear bool) (interface{}, error) {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	if entry == nil {
		panic("raft: Log entry required for error retrieval")
	}

	// If a result exists for the entry then return it with its error.
	if entry.Index > 0 && entry.Index <= uint64(len(l.results)) {
		if result := l.results[entry.Index-1]; result != nil {

			// keep the records before remove it
			returnValue, err := result.returnValue, result.err

			// Remove reference to result if it's being cleared after retrieval.
			if clear {
				result.returnValue = nil
			}

			return returnValue, err
		}
	}

	return nil, nil
}

//--------------------------------------
// Commit
//--------------------------------------

// Retrieves the last index and term that has been committed to the log.
func (l *Log) commitInfo() (index uint64, term uint64) {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	// If we don't have any entries then just return zeros.
	if l.commitIndex == 0 {
		return 0, 0
	}

	// No new commit log after snapshot
	if l.commitIndex == l.startIndex {
		return l.startIndex, l.startTerm
	}

	// Return the last index & term from the last committed entry.
	entry := l.entries[l.commitIndex-1-l.startIndex]
	return entry.Index, entry.Term
}

// Retrieves the last index and term that has been committed to the log.
func (l *Log) lastInfo() (index uint64, term uint64) {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	// If we don't have any entries then just return zeros.
	if len(l.entries) == 0 {
		return l.startIndex, l.startTerm
	}

	// Return the last index & term
	entry := l.entries[len(l.entries)-1]
	return entry.Index, entry.Term
}

// Updates the commit index
func (l *Log) updateCommitIndex(index uint64) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.commitIndex = index
}

// Updates the commit index and writes entries after that index to the stable storage.
func (l *Log) setCommitIndex(index uint64) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if index > l.startIndex+uint64(len(l.entries)) {
		return fmt.Errorf("raft.Log: Commit index (%d) out of range (%d)", index, len(l.entries))
	}

	// Do not allow previous indices to be committed again.

	// This could happens, since the guarantee is that the new leader has up-to-dated
	// log entires rather than has most up-to-dated committed index

	// For example, Leader 1 send log 80 to follower 2 and follower 3
	// follower 2 and follow 3 all got the new entries and reply
	// leader 1 committed entry 80 and send reply to follower 2 and follower3
	// follower 2 receive the new committed index and update committed index to 80
	// leader 1 fail to send the committed index to follower 3
	// follower 3 promote to leader (server 1 and server 2 will vote, since leader 3
	// has up-to-dated the entries)
	// when new leader 3 send heartbeat with committed index = 0 to follower 2,
	// follower 2 should reply success and let leader 3 update the committed index to 80

	if index < l.commitIndex {
		return nil
	}

	// Find all entries whose index is between the previous index and the current index.
	for i := l.commitIndex + 1; i <= index; i++ {
		entryIndex := i - 1 - l.startIndex
		entry := l.entries[entryIndex]

		// Write to storage.
		if err := entry.encode(l.file); err != nil {
			return err
		}

		// Update commit index.
		l.commitIndex = entry.Index

		// Apply the changes to the state machine and store the error code.
		returnValue, err := l.ApplyFunc(entry.Command)
		l.results[entryIndex] = &logResult{returnValue: returnValue, err: err}
	}
	return nil
}

//--------------------------------------
// Truncation
//--------------------------------------

// Truncates the log to the given index and term. This only works if the log
// at the index has not been committed.
func (l *Log) truncate(index uint64, term uint64) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	debugln("log.truncate: ", index)

	// Do not allow committed entries to be truncated.
	if index < l.commitIndex {
		debugln("log.truncate.before")
		return fmt.Errorf("raft.Log: Index is already committed (%v): (IDX=%v, TERM=%v)", l.commitIndex, index, term)
	}

	// Do not truncate past end of entries.
	if index > l.startIndex+uint64(len(l.entries)) {
		debugln("log.truncate.after")
		return fmt.Errorf("raft.Log: Entry index does not exist (MAX=%v): (IDX=%v, TERM=%v)", len(l.entries), index, term)
	}

	// If we're truncating everything then just clear the entries.
	if index == l.startIndex {
		l.entries = []*LogEntry{}
	} else {
		// Do not truncate if the entry at index does not have the matching term.
		entry := l.entries[index-l.startIndex-1]
		if len(l.entries) > 0 && entry.Term != term {
			debugln("log.truncate.termMismatch")
			return fmt.Errorf("raft.Log: Entry at index does not have matching term (%v): (IDX=%v, TERM=%v)", entry.Term, index, term)
		}

		// Otherwise truncate up to the desired entry.
		if index < l.startIndex+uint64(len(l.entries)) {
			debugln("log.truncate.finish")
			l.entries = l.entries[0 : index-l.startIndex]
		}
	}

	return nil
}

//--------------------------------------
// Append
//--------------------------------------

// Appends a series of entries to the log. These entries are not written to
// disk until setCommitIndex() is called.
func (l *Log) appendEntries(entries []*LogEntry) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	// Append each entry but exit if we hit an error.
	for _, entry := range entries {
		if err := l.appendEntry(entry); err != nil {
			return err
		}
	}

	return nil
}

// Writes a single log entry to the end of the log. This function does not
// obtain a lock and should only be used internally. Use AppendEntries() and
// AppendEntry() to use it externally.
func (l *Log) appendEntry(entry *LogEntry) error {
	if l.file == nil {
		return errors.New("raft.Log: Log is not open")
	}

	// Make sure the term and index are greater than the previous.
	if len(l.entries) > 0 {
		lastEntry := l.entries[len(l.entries)-1]
		if entry.Term < lastEntry.Term {
			return fmt.Errorf("raft.Log: Cannot append entry with earlier term (%x:%x <= %x:%x)", entry.Term, entry.Index, lastEntry.Term, lastEntry.Index)
		} else if entry.Term == lastEntry.Term && entry.Index <= lastEntry.Index {
			return fmt.Errorf("raft.Log: Cannot append entry with earlier index in the same term (%x:%x <= %x:%x)", entry.Term, entry.Index, lastEntry.Term, lastEntry.Index)
		}
	}

	// Append to entries list if stored on disk.
	l.entries = append(l.entries, entry)
	l.results = append(l.results, nil)

	return nil
}

//--------------------------------------
// Log compaction
//--------------------------------------

// compaction the log before index
func (l *Log) compact(index uint64, term uint64) error {
	var entries []*LogEntry

	l.mutex.Lock()
	defer l.mutex.Unlock()

	// nothing to compaction
	// the index may be greater than the current index if
	// we just recovery from on snapshot
	if index >= l.internalCurrentIndex() {
		entries = make([]*LogEntry, 0)
	} else {

		// get all log entries after index
		entries = l.entries[index-l.startIndex:]
	}

	// create a new log file and add all the entries
	file, err := os.OpenFile(l.path+".new", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		err = entry.encode(file)
		if err != nil {
			return err
		}
	}
	// close the current log file
	l.file.Close()

	// remove the current log file to .bak
	err = os.Remove(l.path)
	if err != nil {
		return err
	}

	// rename the new log file
	err = os.Rename(l.path+".new", l.path)
	if err != nil {
		return err
	}
	l.file = file

	// compaction the in memory log
	l.entries = entries
	l.startIndex = index
	l.startTerm = term
	return nil
}
