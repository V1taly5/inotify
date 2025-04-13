# inotify

`inotify` — it is a Go library for monitoring changes in the Linux file system using the low-level inotify interface. It is inspired by fsnotify, but provides more direct and flexible access to file system events, including:
- recursive directory tracking (`/path/...`);
- Flag support `inotify`, including `IN_CREATE`, `IN_DELETE`, `IN_MODIFY`, `IN_MOVE_SELF`, `IN_MOVE_TO`, `IN_MOVE_FROM`;
- Automatic addition of new subdirectories when they are created.

## Usage example
```go
package main

import (
    "log"
)

func main() {
    watcher, err := inotify.NewWatcher()
    if err != nil {
        log.Fatal(err)
    }
    defer watcher.Close()

    err = watcher.Add("/path/to/dir/...")
    if err != nil {
        log.Fatal(err)
    }

    for {
        select {
        case event := <-watcher.Events:
            log.Printf("Event: %s %x\n", event.Name, event.Op)
        case err := <-watcher.Errors:
            log.Printf("Error: %v\n", err)
        }
    }
}
```