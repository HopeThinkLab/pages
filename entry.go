package pages

import (
	"errors"
	"io"

	"github.com/NebulousLabs/Sia/build"
)

type (
	// Entry is a single entry in the database. It implements the
	// ReadWriteSeeker interface to enable easy writes to the file
	Entry struct {
		// pm is a pointer to the PageManager that created this Entry
		pm *PageManager

		// ep is the tiered entryPage for this entry
		ep *entryPage

		// cursorOff is the offset of the cursor from the start of the current
		// page it is pointed at
		cursorOff int64

		// cursorPage is the index of the page in pages to which the cursor points
		cursorPage int64
	}
)

// Close is a no-op
func (e *Entry) Close() error {
	e.ep.pm.mu.Lock()
	defer e.ep.pm.mu.Unlock()
	// If the remaining entries pointing to this entryPage is 0 we can delete
	// it from the map
	e.ep.instanceCounter--
	if e.ep.instanceCounter == 0 {
		delete(e.ep.pm.entryPages, Identifier(e.ep.pp.fileOff))
	}
	return nil
}

// read is a helper function that reads at a specific cursorPage and offset
func (e *Entry) read(p []byte, cursorPage *int64, cursorOff *int64) (n int, err error) {
	if len(e.ep.pages) == 0 {
		return 0, io.EOF
	}

	// Get the amount of bytes the caller would like to read
	bytesToRead := int64(len(p))

	// Read until either length bytes were read or until we reached the end of
	// the last page
	copyDest := 0
	readData := make([]byte, bytesToRead)
	for bytesToRead > 0 {
		// Abort if no more pages are left to read
		if *cursorPage >= int64(len(e.ep.pages)) {
			break
		}

		// Read the data from the page
		var bytesRead int
		bytesRead, err = e.ep.pages[*cursorPage].readAt(readData, *cursorOff)
		if err != nil {
			return 0, err
		}

		// Adjust the remaining bytesToRead and the cursor position
		bytesToRead -= int64(bytesRead)
		err = e.seek(int64(bytesRead), cursorPage, cursorOff)
		if err != nil {
			return
		}

		// Copy data to output
		copy(p[copyDest:copyDest+bytesRead], readData)
		copyDest += bytesRead
	}

	// If no data was read signal the EOF
	if copyDest == 0 {
		return 0, io.EOF
	}

	return copyDest, nil
}

// Read tries to read len(p) bytes from the current cursor position
func (e *Entry) Read(p []byte) (n int, err error) {
	e.ep.mu.RLock()
	defer e.ep.mu.RUnlock()
	return e.read(p, &e.cursorPage, &e.cursorOff)
}

// ReadAt reads from a specific offset
func (e *Entry) ReadAt(p []byte, off int64) (int, error) {
	e.ep.mu.RLock()
	defer e.ep.mu.RUnlock()

	// Seek to the offset from the beginning of the file
	cursorPage := int64(0)
	cursorOff := int64(0)
	if err := e.seek(off, &cursorPage, &cursorOff); err != nil {
		return 0, err
	}

	// Read the data
	return e.read(p, &cursorPage, &cursorOff)
}

// seek is a helper function that seeks a specific offset starting at a
// specified cursorPage and cursorOffset. It doesn't modify the Entry's fields
// but instead the input values
func (e *Entry) seek(offset int64, cursorPage *int64, cursorOff *int64) error {
	// Don't allow to seek before start of file
	if *cursorPage*pageSize+*cursorOff+offset < 0 {
		return errors.New("Cannot set cursor to negative position")
	}

	cursorPageNew := (*cursorPage*pageSize + *cursorOff + offset) / pageSize
	cursorOffNew := (*cursorPage*pageSize + *cursorOff + offset) % pageSize

	// If the page number is higher than the number of available pages set it to
	// the number of available pages at offset 0 to signal other functions that
	// we cannot continue reading
	if cursorPageNew >= int64(len(e.ep.pages)) {
		cursorPageNew = int64(len(e.ep.pages))
		cursorOffNew = 0
	}

	*cursorPage = cursorPageNew
	*cursorOff = cursorOffNew
	return nil
}

// Seek moves the cursor for reading and writing to the appropriate page and
// offset
func (e *Entry) Seek(offset int64, whence int) (int64, error) {
	e.ep.mu.RLock()
	defer e.ep.mu.RUnlock()

	// Calculate the correct page and page offset
	var pageNum int64
	var pageOff int64

	switch whence {
	case io.SeekStart:
		pageNum = 0
		pageOff = 0
	case io.SeekCurrent:
		pageNum = e.cursorPage
		pageOff = e.cursorOff
	case io.SeekEnd:
		pageNum = int64(len(e.ep.pages))
		pageOff = 0
	}

	err := e.seek(offset, &pageNum, &pageOff)
	if err != nil {
		return 0, err
	}

	e.cursorPage = pageNum
	e.cursorOff = pageOff

	return e.cursorPage*pageSize + e.cursorOff, nil
}

// Sync calls sync on the underlying file of the Page Manager
func (e *Entry) Sync() error {
	return e.pm.file.Sync()
}

// Truncate shortens an entry to size bytes
func (e *Entry) Truncate(size int64) error {
	e.ep.mu.Lock()
	defer e.ep.mu.Unlock()

	// Recursively truncate the tree
	_, pagesToFree1, err := e.ep.recursiveTruncate(e.ep.root, size)
	if err != nil {
		return err
	}

	// Defrag the tree afterwards
	pagesToFree2, err := e.ep.defrag()
	if err != nil {
		return err
	}

	// Free pages
	return e.pm.freePages.addPages(append(pagesToFree1, pagesToFree2...))
}

// write is a helper function that writes at a specific cursorPage and offset
func (e *Entry) write(p []byte, cursorPage *int64, cursorOff *int64) (int, error) {
	// Get the amount of bytes the caller would like to write
	bytesToWrite := int64(len(p))

	// Inform the entryPage about new pages and the increase data usage
	byteIncrease := int64(0)
	addedPages := make([]*physicalPage, 0)

	// backup cursorPage and cursorOff in case we need to reset the loop
	bCursorPage := *cursorPage
	bCursorOff := *cursorOff

	// Write until all the bytes are written. If necessary allocate new pages
	writeCursor := 0
	appending := false
	for bytesToWrite > 0 {
		// Check if we are going to add a new page or extend the last page
		if !appending &&
			(*cursorPage >= int64(len(e.ep.pages)) ||
				(*cursorPage == int64(len(e.ep.pages)-1) &&
					*cursorOff+bytesToWrite > e.ep.pages[*cursorPage].usedSize)) {
			// Seems like we are appending now. Change to write lock and
			// restart loop.
			appending = true
			e.ep.mu.RUnlock()
			e.ep.mu.Lock()
			defer e.ep.mu.RLock()
			defer e.ep.mu.Unlock()

			// Reset loop
			*cursorPage = bCursorPage
			*cursorOff = bCursorOff
			bytesToWrite = int64(len(p))
			writeCursor = 0
			byteIncrease = int64(0)
			addedPages = make([]*physicalPage, 0)
			continue
		}

		if *cursorPage >= int64(len(e.ep.pages)) {
			// Allocate new page if necessary
			newPage, err := e.pm.managedAllocatePage()
			if err != nil {
				return 0, err
			}
			// Add it to the list of pages and addedPages
			addedPages = append(addedPages, newPage)
			e.ep.pages = append(e.ep.pages, newPage)

			// If we still don't have enough pages mark this page as full
			if *cursorPage >= int64(len(e.ep.pages)) {
				newPage.usedSize = pageSize
				byteIncrease += pageSize
			}
			continue
		}

		// Write parts of the data to the page and remember the size increase
		// of the page
		page := e.ep.pages[*cursorPage]
		usedPageSize := page.usedSize
		bytesWritten, err := page.writeAt(p[writeCursor:], *cursorOff)
		byteIncrease += (page.usedSize - usedPageSize)
		if err != nil {
			return 0, err
		}

		// Adjust the remaining bytesToWrite and the cursor position
		bytesToWrite -= int64(bytesWritten)
		err = e.seek(int64(bytesWritten), cursorPage, cursorOff)
		if err != nil {
			return 0, err
		}

		// Increment the writeCursor of the input data
		writeCursor += bytesWritten
	}
	err := e.ep.addPages(addedPages, byteIncrease)
	if err != nil {
		return 0, build.ExtendErr("failed to add pages to entryPage", err)
	}

	return len(p), nil
}

// Write tries to write len(p) byte to the current cursor position
func (e *Entry) Write(p []byte) (int, error) {
	e.ep.mu.RLock()
	defer e.ep.mu.RUnlock()
	return e.write(p, &e.cursorPage, &e.cursorOff)
}

// WriteAt writes to a specific offset
func (e *Entry) WriteAt(p []byte, off int64) (n int, err error) {
	e.ep.mu.RLock()
	defer e.ep.mu.RUnlock()

	// Seek to the offset from the beginning of the file
	cursorPage := int64(0)
	cursorOff := int64(0)
	if err := e.seek(off, &cursorPage, &cursorOff); err != nil {
		return 0, err
	}

	// Write data
	return e.write(p, &cursorPage, &cursorOff)
}
