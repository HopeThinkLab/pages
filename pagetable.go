package pages

import (
	"encoding/binary"

	"github.com/NebulousLabs/Sia/build"
)

type (
	// pageTable is used to find pages associated with a certain group of
	// pages. It can either point to pages or to other pageTables not both.
	pageTable struct {
		// height indicates how far the pageTable is from the bottom layer of
		// the pageTable tree. A height of 0 indicates that the children of this
		// pageTable are pages
		height int64

		// parent points to the parent of the pageTable
		parent *pageTable

		// children are the pageTables the current pageTable is pointing to.
		childTables map[uint64]*pageTable

		// childPages are the physical pages the currente pageTable is pointing to.
		childPages map[uint64]*physicalPage

		// pp is the physical page on which the pageTable is stored
		pp *physicalPage
	}
)

// newPageTable is a helper function to create a pageTable
func newPageTable(height int64, parent *pageTable, pm *PageManager) (*pageTable, error) {
	// Allocate a page for the table
	pp, err := pm.allocatePage()
	if err != nil {
		return nil, build.ExtendErr("failed to allocate page for new pageTable", err)
	}

	// Create and return the table
	pt := pageTable{
		parent:      parent,
		height:      height,
		pp:          pp,
		childPages:  make(map[uint64]*physicalPage),
		childTables: make(map[uint64]*pageTable),
	}
	return &pt, nil
}

// extendPageTableTree extends the pageTable tree by creating a new root,
// adding the current root as the first child and creating the rest of the tree
// structure
func extendPageTableTree(root *pageTable, pm *PageManager) (*pageTable, error) {
	if root.parent != nil {
		// This should only ever be called on the root node
		panic("Sanity check failed. Pt is not the root node")
	}

	// Create a new root pageTable
	newRoot, err := newPageTable(root.height+1, nil, pm)
	if err != nil {
		return nil, build.ExtendErr("Failed to create new pageTable to extend the tree", err)
	}

	// Set the previous root pageTable to be the child of the new one
	newRoot.childTables[0] = root
	root.parent = newRoot

	return newRoot, nil
}

// marshal serializes a pageTable to be able to write it to disk
func (pt pageTable) marshal() ([]byte, error) {
	// Get the number of entries and the offsets of the entries
	var numEntries uint64
	var offsets []int64
	if pt.height == 0 {
		numEntries = uint64(len(pt.childPages))
		for i := uint64(0); i < numEntries; i++ {
			offsets = append(offsets, pt.childPages[uint64(i)].fileOff)
		}
	} else {
		numEntries = uint64(len(pt.childTables))
		for i := uint64(0); i < numEntries; i++ {
			offsets = append(offsets, pt.childTables[uint64(i)].pp.fileOff)
		}
	}

	// off is an offset used for marshalling the data
	off := 0

	// Allocate enough memory for marshalled data
	data := make([]byte, (numEntries+1)*8)

	// Write the number of entries
	binary.LittleEndian.PutUint64(data[off:8], numEntries)
	off += 8

	// Write the offsets of the entries
	for _, offset := range offsets {
		binary.PutVarint(data[off:off+8], offset)
		off += 8
	}

	return data, nil
}

// writeToDisk marshals a pageTable and writes it to disk
func (pt pageTable) writeToDisk() error {
	// Marshal the pageTable
	data, err := pt.marshal()
	if err != nil {
		return build.ExtendErr("Failed to marshal pageTable", err)
	}

	// Write it to disk
	_, err = pt.pp.writeAt(data, 0)
	if err != nil {
		return build.ExtendErr("Failed to write pageTable to disk", err)
	}
	return nil
}

// Size returns the length of the pageTable if it was marshalled
func (pt pageTable) Size() uint32 {
	// 4 Bytes for the tableType
	// 4 Bytes for the pageOffsts length
	// 8 * children bytes for the elements
	var children uint32
	if pt.height == 0 {
		children = uint32(len(pt.childPages))
	} else {
		children = uint32(len(pt.childTables))
	}
	return 4 + 4 + 8*children
}
