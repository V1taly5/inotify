package inotify

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TODO
// type Op uint32 for unix.IN_ANY
const (
	Create     = unix.IN_CREATE
	Write      = unix.IN_MODIFY
	Remove     = unix.IN_DELETE
	Rename     = unix.IN_MOVED_TO | unix.IN_MOVED_FROM
	Movedto    = unix.IN_MOVED_TO
	Movedfrom  = unix.IN_MOVED_FROM
	Chmod      = unix.IN_ATTRIB
	CloseWrite = unix.IN_CLOSE_WRITE
)

var (
	ErrNonExistentWatch = errors.New("inotify: can't remove non-existent watch")
	ErrClosed           = errors.New("inotify: watcher already closed")
	ErrEventOverflow    = errors.New("inotify: queue or buffer overflow")
	ErrInvalildFD       = errors.New("inotify: invalid file descriptor")
)

type Inotify struct {
	Events chan Event
	Errors chan error

	fd          int
	inotifyFile *os.File

	watches *watches
	mu      sync.Mutex

	done     chan struct{}
	doneResp chan struct{}
}

type Event struct {
	Name string
	Op   uint32
}

type watches struct {
	wd   map[uint32]*watch // wd -> watch
	path map[string]uint32 // path -> wd
}

type watch struct {
	wd      uint32
	flags   uint32
	path    string
	recurse bool
}

func NewWatches() *watches {
	return &watches{
		wd:   make(map[uint32]*watch),
		path: make(map[string]uint32),
	}
}

func (w *watches) byWd(wd uint32) *watch {
	return w.wd[wd]
}

func (w *watches) byPath(path string) *watch {
	return w.wd[w.path[path]]
}

func (w *watches) len() int {
	return len(w.wd)
}

func (w *watches) add(ww *watch) {
	w.wd[ww.wd] = ww
	w.path[ww.path] = ww.wd
}

func (w *watches) remove(ww *watch) {
	delete(w.path, ww.path)
	delete(w.wd, ww.wd)
}

func (w *watches) removePath(path string) ([]uint32, error) {
	path, recurse := recursivePath(path)
	wd, ok := w.path[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNonExistentWatch, path)
	}

	watch := w.wd[wd]
	if recurse && !watch.recurse {
		return nil, fmt.Errorf("can't use /... with non-recursive watch %q", path)
	}

	w.remove(watch)
	// delete(w.path, path)
	// delete(w.wd, wd)
	if !watch.recurse {
		return []uint32{wd}, nil
	}

	wds := make([]uint32, 0, 8)
	wds = append(wds, wd)
	for p, rwd := range w.path {
		if strings.HasPrefix(p, path) {
			// w.remove(watch)
			delete(w.path, p)
			delete(w.wd, rwd)
			wds = append(wds, rwd)
		}
	}
	return wds, nil
}

func (w *watches) updatePath(path string, f func(*watch) (*watch, error)) error {
	var existing *watch
	wd, ok := w.path[path]
	if ok {
		existing = w.wd[wd]
	}

	upd, err := f(existing)
	if err != nil {
		return err
	}
	if upd != nil {
		w.add(upd)
		// w.wd[upd.wd] = upd
		// w.path[upd.path] = upd.wd

		if upd.wd != wd {
			delete(w.wd, wd)
		}
	}

	return nil
}

func NewWatcher() (*Inotify, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if fd == -1 {
		return nil, err
	}

	i := &Inotify{
		Events: make(chan Event),
		Errors: make(chan error),

		fd:          fd,
		inotifyFile: os.NewFile(uintptr(fd), ""),

		watches: NewWatches(),

		done:     make(chan struct{}),
		doneResp: make(chan struct{}),
	}

	go i.readEvents()
	return i, err
}

func (w *Inotify) register(path string, flags uint32, recurse bool) error {
	return w.watches.updatePath(path, func(existing *watch) (*watch, error) {
		if existing != nil {
			flags |= existing.flags | unix.IN_MASK_ADD
		}

		wd, err := unix.InotifyAddWatch(w.fd, path, flags)
		if wd == -1 {
			return nil, err
		}

		if e, ok := w.watches.wd[uint32(wd)]; ok {
			return e, nil
		}

		if existing == nil {
			return &watch{
				wd:      uint32(wd),
				path:    path,
				flags:   flags,
				recurse: recurse,
			}, nil
		}

		existing.wd = uint32(wd)
		existing.flags = flags
		return existing, nil
	})
}

func (w *Inotify) Add(name string) error {
	return w.AddWith(name, Create|Write|Remove|Rename|Chmod|CloseWrite)
}

func (w *Inotify) AddWith(path string, flags uint32) error {
	select {
	case <-w.done:
		return ErrClosed
	default:
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	path, recurse := recursivePath(path)
	if recurse {
		return filepath.WalkDir(path, func(root string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if root == path {
					return fmt.Errorf("inotify: not a directory: %q", path)
				}
				return nil
			}
			return w.register(root, flags, true)
		})
	}
	return w.register(path, flags, false)
}

func (w *Inotify) Close() error {
	close(w.done)
	err := w.inotifyFile.Close()
	if err != nil {
		return err
	}
	<-w.doneResp // wait for readEvents() to finish
	return nil
}

func (w *Inotify) Remove(name string) error {
	select {
	case <-w.done:
		return ErrClosed
	default:
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	return w.remove(filepath.Clean(name))
}

func (w *Inotify) remove(name string) error {
	wds, err := w.watches.removePath(name)
	if err != nil {
		return err
	}

	for _, wd := range wds {
		_, err := unix.InotifyRmWatch(w.fd, wd)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				switch errno {
				case syscall.EBADF:
					return ErrInvalildFD
				case syscall.EINVAL:
					continue
				default:
					return fmt.Errorf("unexpected error removing watch: %w", err)
				}
			} else {
				return fmt.Errorf("unexpected error type: %w", err)
			}
		}
	}
	return nil
}

func (w *Inotify) readEvents() {
	defer func() {
		close(w.Errors)
		close(w.Events)
		close(w.doneResp)
	}()

	var buf [unix.SizeofInotifyEvent * 4096]byte
	for {
		select {
		case <-w.done:
			return
		default:
		}

		n, err := w.inotifyFile.Read(buf[:])
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			if !w.sendError(err) {
				return
			}
			continue
		}

		if n < unix.SizeofInotifyEvent {
			// read was too short
			err := errors.New("inotify: short read in readEvents()")
			if n == 0 {
				err = io.EOF // if EOF is received -> this should really never happen
			}
			if !w.sendError(err) {
				return
			}
			continue
		}

		// we don't know how many events we just read into the buffer while the
		// offset points to at least one whole event
		var offset uint32
		for offset <= uint32(n-unix.SizeofInotifyEvent) {
			// point to the event in the buffer
			inEvent := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))

			if inEvent.Mask&unix.IN_Q_OVERFLOW != 0 {
				if !w.sendError(ErrEventOverflow) {
					return
				}
			}

			ev, ok := w.handleEvent(inEvent, &buf, offset)
			if !ok {
				return
			}

			// TODO: move to handleEvent
			if ev.Op&unix.IN_CREATE == unix.IN_CREATE || ev.Op&unix.IN_MOVED_TO == unix.IN_MOVED_TO {
				info, err := os.Stat(ev.Name)
				if err != nil {
					if !w.sendError(err) {
						return
					}
					continue
				}
				if info.IsDir() {
					if err := w.Add(ev.Name); err != nil {
						if !w.sendError(err) {
							return
						}

					}
				}
				// if err == nil && info.IsDir() {
				// 	w.AddWith(ev.Name, Create|Write|Remove|Rename|Chmod|CloseWrite)
				// }
			}
			if !w.sendEvent(ev) {
				return
			}

			// move to the next event in the buffer
			offset += unix.SizeofInotifyEvent + inEvent.Len
		}
	}

}
func recursivePath(path string) (string, bool) {
	path = filepath.Clean(path)
	if filepath.Base(path) == "..." {
		return filepath.Dir(path), true
	}
	return path, false
}

func (w *Inotify) sendError(err error) bool {
	if err == nil {
		return true
	}
	select {
	case <-w.done:
		return false
	case w.Errors <- err:
		return true
	}
}

func (w *Inotify) sendEvent(e Event) bool {
	if e.Op == 0 {
		return true
	}
	select {
	case <-w.done:
		return false
	case w.Events <- e:
		return true
	}
}
