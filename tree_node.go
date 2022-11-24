// Copyright 2022 bnb-chain. All Rights Reserved.
//
// Distributed under MIT license.
// See file LICENSE for detail or copy at https://opensource.org/licenses/MIT

package bsmt

import (
	"bytes"
	"fmt"
	"github.com/panjf2000/ants/v2"
	"runtime"
	"sort"
	"strconv"
	"sync"
)

const (
	hashSize    = 32
	versionSize = 40
)

func NewTreeNode(depth uint8, path uint64, nilHashes *nilHashes, hasher *Hasher) *TreeNode {
	treeNode := &TreeNode{
		nilHash:      nilHashes.Get(depth),
		nilChildHash: nilHashes.Get(depth + 4),
		path:         path,
		depth:        depth,
		hasher:       hasher,
		internalMu:   make([]sync.RWMutex, 14),
		internalVer:  make([]Version, 14),
	}
	for i := 0; i < 2; i++ {
		treeNode.Internals[i] = nilHashes.Get(depth + 1)
	}
	for i := 2; i < 6; i++ {
		treeNode.Internals[i] = nilHashes.Get(depth + 2)
	}
	for i := 6; i < 14; i++ {
		treeNode.Internals[i] = nilHashes.Get(depth + 3)
	}

	return treeNode
}

type InternalNode []byte

type TreeNode struct {
	mu        sync.RWMutex
	Children  [16]*TreeNode
	Internals [14]InternalNode
	Versions  []*VersionInfo

	nilHash      []byte
	nilChildHash []byte
	path         uint64
	depth        uint8
	hasher       *Hasher
	temporary    bool
	internalMu   []sync.RWMutex
	internalVer  []Version
}

// Root Get latest hash of a node
func (node *TreeNode) Root() []byte {
	node.mu.RLock()
	defer node.mu.RUnlock()

	if len(node.Versions) == 0 {
		return node.nilHash
	}
	return node.Versions[len(node.Versions)-1].Hash
}

// Root Get latest hash of a node without a lock
func (node *TreeNode) root() []byte {
	if len(node.Versions) == 0 {
		return node.nilHash
	}
	return node.Versions[len(node.Versions)-1].Hash
}

func (node *TreeNode) Set(hash []byte, version Version) {
	node.mu.Lock()
	defer node.mu.Unlock()

	node.newVersion(&VersionInfo{
		Ver:  version,
		Hash: hash,
	})
}

func (node *TreeNode) newVersion(version *VersionInfo) {
	if len(node.Versions) > 0 && node.Versions[len(node.Versions)-1].Ver == version.Ver {
		// a new version already exists, overwrite it
		node.Versions[len(node.Versions)-1] = version
		return
	}
	node.Versions = append(node.Versions, version)
}

func (node *TreeNode) SetChildrenOnly(child *TreeNode, nibble int, version Version) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.Children[nibble] = child
}

func (node *TreeNode) SetChildren(child *TreeNode, nibble int, version Version) {
	node.mu.Lock()
	defer node.mu.Unlock()

	node.Children[nibble] = child
	//fmt.Printf("Computing child %d....\n", nibble)

	left, right := node.nilChildHash, node.nilChildHash
	switch nibble % 2 {
	case 0:
		if node.Children[nibble] != nil {
			left = node.Children[nibble].Root()
		}
		if node.Children[nibble^1] != nil {
			right = node.Children[nibble^1].Root()
		}
		//fmt.Printf("Will compute leaf %d , %d\n", nibble, nibble^1)
	case 1:
		if node.Children[nibble] != nil {
			right = node.Children[nibble].Root()
		}
		if node.Children[nibble^1] != nil {
			left = node.Children[nibble^1].Root()
		}
		//fmt.Printf("Will compute leaf %d , %d\n", nibble^1, nibble)
	}
	prefix := 6
	for i := 4; i >= 1; i >>= 1 {
		nibble = nibble / 2
		node.Internals[prefix+nibble] = node.hasher.Hash(left, right)
		switch nibble % 2 {
		case 0:
			left = node.Internals[prefix+nibble]
			right = node.Internals[prefix+nibble^1]
			//fmt.Printf("Will compute %d , %d\n", prefix+nibble, prefix+nibble^1)
		case 1:
			right = node.Internals[prefix+nibble]
			left = node.Internals[prefix+nibble^1]
			//fmt.Printf("Will compute %d , %d\n", prefix+nibble^1, prefix+nibble)
		}
		prefix = prefix - i
	}
	// update current root node
	node.newVersion(&VersionInfo{
		Ver:  version,
		Hash: node.hasher.Hash(node.Internals[0], node.Internals[1]),
	})
}

// Recompute all internal hashes
func (node *TreeNode) ComputeInternalHash() {
	node.mu.Lock()
	defer node.mu.Unlock()

	// leaf node
	for i := 0; i < 15; i += 2 {
		left, right := node.nilChildHash, node.nilChildHash
		if node.Children[i] != nil {
			left = node.Children[i].Root()
		}
		if node.Children[i+1] != nil {
			right = node.Children[i+1].Root()
		}
		node.Internals[6+i/2] = node.hasher.Hash(left, right)
	}
	// internal node
	for i := 13; i > 1; i -= 2 {
		node.Internals[i/2-1] = node.hasher.Hash(node.Internals[i-1], node.Internals[i])
	}
}

func (node *TreeNode) Copy() *TreeNode {
	node.mu.RLock()
	defer node.mu.RUnlock()

	return &TreeNode{
		Children:     node.Children,
		Internals:    node.Internals,
		Versions:     node.Versions,
		nilHash:      node.nilHash,
		nilChildHash: node.nilChildHash,
		path:         node.path,
		depth:        node.depth,
		hasher:       node.hasher,
		temporary:    node.temporary,
		internalMu:   node.internalMu,
		internalVer:  node.internalVer,
	}
}

func (node *TreeNode) mark(nibble int) {
	//node.mu.Lock()
	//defer node.mu.Unlock()
	for _, i := range leafInternalMap[nibble] {
		node.Internals[i] = nil
	}
}

func (node *TreeNode) Prune(oldestVersion Version) uint64 {
	node.mu.Lock()
	defer node.mu.Unlock()

	if len(node.Versions) <= 1 {
		return 0
	}
	i := 0
	for ; i < len(node.Versions)-1; i++ {
		if node.Versions[i].Ver >= oldestVersion {
			break
		}
	}

	originSize := len(node.Versions) * versionSize
	if i > 0 && node.Versions[i].Ver > oldestVersion {
		node.Versions = node.Versions[i-1:]
		return uint64(originSize - len(node.Versions)*versionSize)
	}

	node.Versions = node.Versions[i:]
	return uint64(originSize - len(node.Versions)*versionSize)
}

func (node *TreeNode) Rollback(targetVersion Version) (bool, uint64) {
	node.mu.Lock()
	defer node.mu.Unlock()

	if len(node.Versions) == 0 {
		return false, 0
	}
	var next bool
	originSize := len(node.Versions) * versionSize
	i := len(node.Versions) - 1
	for ; i >= 0; i-- {
		if node.Versions[i].Ver <= targetVersion {
			break
		}
		next = true
	}
	node.Versions = node.Versions[:i+1]
	return next, uint64(originSize - len(node.Versions)*versionSize)
}

// The node has not been updated for a long time,
// the subtree is emptied, and needs to be re-read from the database when it needs to be modified.
func (node *TreeNode) archive() {
	for i := 0; i < len(node.Internals); i++ {
		node.Internals[i] = nil
	}
	for i := 0; i < len(node.Children); i++ {
		node.Children[i] = nil
	}
	node.temporary = true
}

// PreviousVersion returns the previous version number in the current TreeNode
func (node *TreeNode) PreviousVersion() Version {
	node.mu.RLock()
	defer node.mu.RUnlock()

	if len(node.Versions) <= 1 {
		return 0
	}
	return node.Versions[len(node.Versions)-2].Ver
}

// size returns the current node size
func (node *TreeNode) Size() uint64 {
	if node.temporary {
		return uint64(len(node.Versions) * versionSize)
	}
	return uint64(len(node.Versions)*versionSize + hashSize*len(node.Internals))
}

// Release nodes that have not been updated for a long time from memory.
// slowing down memory usage in runtime.
func (node *TreeNode) Release(oldestVersion Version) uint64 {
	node.mu.Lock()
	defer node.mu.Unlock()

	size := node.Size()
	for i := 0; i < len(node.Children); i++ {
		if node.Children[i] != nil {
			length := len(node.Children[i].Versions)
			if length > 0 && node.Children[i].Versions[length-1].Ver < oldestVersion {
				// check for the latest version and release it if it is older than the pruned version
				node.Children[i].archive()
				size += node.Children[i].Size()
			} else {
				size += node.Children[i].Release(oldestVersion)
			}
		}
	}
	return size
}

// The nodes without child data.
// will be extended when it needs to be searched down.
func (node *TreeNode) IsTemporary() bool {
	return node.temporary
}

func (node *TreeNode) ToStorageTreeNode() *StorageTreeNode {
	node.mu.RLock()
	defer node.mu.RUnlock()

	var children [16]*StorageLeafNode
	for i := 0; i < 16; i++ {
		if node.Children[i] != nil {
			children[i] = &StorageLeafNode{node.Children[i].Versions}
		}
	}
	return &StorageTreeNode{
		Children:  children,
		Internals: node.Internals,
		Versions:  node.Versions,
		Path:      node.path,
	}
}

func (node *TreeNode) computeInternal(nibbles map[uint64]struct{}, pool *ants.Pool) {
	if nibbles == nil {
		return
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	//fmt.Println("computing child ", node.path)
	nbArray := make([]uint64, 0, len(nibbles))
	for nibble := range nibbles {
		nbArray = append(nbArray, nibble)
	}
	sort.Slice(nbArray, func(i, j int) bool { return nbArray[i] > nbArray[j] })

	prefix := 6
	for i := 4; i >= 1; i >>= 1 {
		wg := sync.WaitGroup{}
		for _, n := range nbArray {
			if int(n) >= prefix && int(n) <= prefix<<1+1 {
				wg.Add(1)
				func(ni uint64) {
					_ = pool.Submit(func() {
						defer wg.Done()
						left, right := node.childrenHash(ni)
						node.Internals[ni] = node.hasher.Hash(left, right)
					})
				}(n)
			}
		}
		wg.Wait()
		prefix = prefix - i
	}

	// update current root node
	node.newVersion(&VersionInfo{
		Ver:  node.latestVersion(),
		Hash: node.hasher.Hash(node.Internals[0], node.Internals[1]),
	})
}

type nibbles []uint64

type VersionInfo struct {
	Ver  Version
	Hash []byte
}

type StorageLeafNode struct {
	Versions []*VersionInfo `rlp:"optional"`
}

type StorageTreeNode struct {
	Children  [16]*StorageLeafNode `rlp:"optional"`
	Internals [14]InternalNode     `rlp:"optional"`
	Versions  []*VersionInfo       `rlp:"optional"`
	Path      uint64               `rlp:"optional"`
}

func (node *StorageTreeNode) ToTreeNode(depth uint8, nilHashes *nilHashes, hasher *Hasher) *TreeNode {
	treeNode := &TreeNode{
		Internals:    node.Internals,
		Versions:     node.Versions,
		nilHash:      nilHashes.Get(depth),
		nilChildHash: nilHashes.Get(depth + 4),
		path:         node.Path,
		depth:        depth,
		hasher:       hasher,
	}
	for i := 0; i < 16; i++ {
		if node.Children[i] != nil && len(node.Children[i].Versions) > 0 {
			treeNode.Children[i] = &TreeNode{
				Versions:     node.Children[i].Versions,
				nilHash:      nilHashes.Get(depth + 4),
				nilChildHash: nilHashes.Get(depth + 8),
				hasher:       hasher,
				temporary:    true,
			}
		}
	}

	return treeNode
}

func (node *TreeNode) latestVersion() Version {
	if len(node.Versions) <= 0 {
		return 0
	}
	return node.Versions[len(node.Versions)-1].Ver
}

func (node *TreeNode) latestVersionWithLock() Version {
	node.mu.RLock()
	defer node.mu.RUnlock()
	if len(node.Versions) <= 0 {
		return 0
	}
	return node.Versions[len(node.Versions)-1].Ver
}

func (node *TreeNode) childrenHash(nibble uint64) (left, right []byte) {
	//orig := nibble
	if nibble >= 6 {
		// find child in leaves
		// 6: 14(0), 15(1), 8: 18(4), 19(5)
		nibble <<= 1
		nibble = nibble + 2 - 14
		left, right = node.nilChildHash, node.nilChildHash
		if node.Children[nibble] != nil {
			left = node.Children[nibble].Root()
		}
		if node.Children[nibble^1] != nil {
			right = node.Children[nibble^1].Root()
		}
	} else {
		// 2: 6, 7      3: 8,9
		nibble = nibble<<1 + 2
		left = node.Internals[nibble]
		right = node.Internals[nibble^1]
	}
	//fmt.Printf("find children of %d: %d, %d\n", orig, nibble, nibble^1)
	return
}

// recompute inner node
func (node *TreeNode) recompute(child *TreeNode, journals *journal, version Version) bool {
	//node.Children[child.path&0xf] = child
	//node.mu.Lock()
	//defer node.mu.Unlock()
	// for all children, recompute hash in parallel
	//fmt.Printf("%d, calc parent: %d-%d, child: %d-%d\n", getGID(), node.depth, node.path, child.depth, child.path)

	nibble := int(child.path & 0xf)
	//sibling := node.Children[nibble^1]
	left, right := node.nilChildHash, node.nilChildHash
	// if sibling haven't finished yet,quit; sibling will be charge for computing
	switch nibble % 2 {
	case 0:
		left = child.root()
		if sibling, exist := journals.get(journalKey{child.depth, child.path ^ 1}); exist {
			if sibling.latestVersionWithLock() < version {
				//fmt.Printf("sibling %d-%d is not latest, return\n", sibling.depth, sibling.path)
				return false
			}
			right = sibling.root()
		}
		//if sibling == nil {
		//	return
		//} else {
		//	if sibling.latestVersionWithLock() < version {
		//		return
		//	} else {
		//		right = sibling.root()
		//	}
		//}
	case 1:
		right = child.root()
		if sibling, exist := journals.get(journalKey{child.depth, child.path ^ 1}); exist {
			if sibling.latestVersionWithLock() < version {
				//fmt.Printf("sibling %d-%d is not latest, return\n", sibling.depth, sibling.path)
				return false
			}
			left = sibling.root()
		}
		//if sibling == nil {
		//	return
		//} else {
		//	if sibling.latestVersionWithLock() < version {
		//		return
		//	} else {
		//		left = sibling.root()
		//	}
		//}
	}
	prefix := 6
	for i := 4; i >= 1; i >>= 1 {
		nibble = nibble / 2
		hash, setBefore := node.setInternal(prefix+nibble, left, right, version)
		if setBefore {
			return false
		}
		//if hash == nil {
		//	return
		//}
		//node.Internals[prefix+nibble] = node.hasher.Hash(left, right)

		switch nibble % 2 {
		case 0:
			siblingNibble := prefix + nibble ^ 1
			siblingHash := node.getInternal(siblingNibble, version)
			if siblingHash == nil {
				//fmt.Printf("%d Internal sibling %d is nil, node %d-%d return\n", getGID(), siblingNibble, node.depth, node.path)
				return false
			}
			left = hash
			right = siblingHash
		case 1:
			siblingNibble := prefix + nibble ^ 1
			siblingHash := node.getInternal(siblingNibble, version)
			if siblingHash == nil {
				//fmt.Printf("%d Internal sibling %d is nil, node %d-%d return\n", getGID(), siblingNibble, node.depth, node.path)
				return false
			}
			right = hash
			left = siblingHash
		}
		prefix = prefix - i
	}
	// update current root
	node.newVersion(&VersionInfo{
		Ver:  version,
		Hash: node.hasher.Hash(node.Internals[0], node.Internals[1]),
	})
	return true
	//node.newVersion(&VersionInfo{
	//	Ver:  version,
	//	Hash: node.hasher.Hash(node.Internals[0], node.Internals[1]),
	//})

	//for depth != 0 {
	//	if leaf {
	//		left, right := node.Children[i], node.Children[i^1]
	//	} else {
	//		node.internalNode[i].RLock()
	//		left, right := node.Internals[i], node.Internals[i^1]
	//		node.internalNode[i].RUnlock()
	//		if left != latestVersion || right != latestVersion {
	//			return
	//		}
	//	}
	//	node.internalNode[x].Lock()
	//	node.Internals[x] = node.hasher.Hash(left, right)
	//	node.internalNode[x].Unlock()
	//}
	//if already calc root {
	//	cancel()
	//}

	//nibble := int(child.path & 0xf)
	//node.Children[nibble] = child
	//left, right := node.nilChildHash, node.nilChildHash
	//// if sibling haven't finished yet,quit; sibling will be charge for computing
	//switch nibble % 2 {
	//case 0:
	//	left = child.root()
	//	if sibling, exist := journals.get(journalKey{child.depth, child.path ^ 1}); exist {
	//		if version > sibling.latestVersionWithLock() {
	//			return
	//		}
	//		right = sibling.root()
	//	}
	//case 1:
	//	right = child.root()
	//	if sibling, exist := journals.get(journalKey{child.depth, child.path ^ 1}); exist {
	//		if version > sibling.latestVersionWithLock() {
	//			return
	//		}
	//		left = sibling.root()
	//	}
	//}
	//
	//prefix := 6
	//for i := 4; i >= 1; i >>= 1 {
	//	nibble = nibble / 2
	//	node.Internals[prefix+nibble] = node.hasher.Hash(left, right)
	//	switch nibble % 2 {
	//	case 0:
	//		left = node.Internals[prefix+nibble]
	//		right = node.Internals[prefix+nibble^1]
	//	case 1:
	//		right = node.Internals[prefix+nibble]
	//		left = node.Internals[prefix+nibble^1]
	//	}
	//	prefix = prefix - i
	//}
	//// update current root
	//node.newVersion(&VersionInfo{
	//	Ver:  version,
	//	Hash: node.hasher.Hash(node.Internals[0], node.Internals[1]),
	//})
}

func (node *TreeNode) setInternal(idx int, left []byte, right []byte, version Version) ([]byte, bool) {
	node.internalMu[idx].Lock()
	defer node.internalMu[idx].Unlock()
	if node.Internals[idx] != nil {
		return node.Internals[idx], true
	}
	hash := node.hasher.Hash(left, right)
	node.Internals[idx] = hash
	node.internalVer[idx] = version
	return hash, false
}

func (node *TreeNode) getInternal(idx int, version Version) []byte {
	node.internalMu[idx].RLock()
	defer node.internalMu[idx].RUnlock()
	return node.Internals[idx]
}

func (node *TreeNode) getChild(nibble int) *TreeNode {
	node.mu.RLock()
	defer node.mu.RUnlock()
	return node.Children[nibble]
}

func (node *TreeNode) newVersionWithLock(version *VersionInfo) {
	node.mu.Lock()
	defer node.mu.Unlock()
	l := len(node.Versions)
	if l == 0 || version.Ver > node.Versions[l-1].Ver {
		fmt.Printf("%d, setting root: %d-%d\n", getGID(), node.depth, node.path)
		node.Versions = append(node.Versions, version)
	}
}

type internalNode struct {
	mu     sync.RWMutex
	ver    Version
	nibble int
}

var leafInternalMap = map[int][]int{
	0:  {0, 2, 6},
	1:  {0, 2, 6},
	2:  {0, 2, 7},
	3:  {0, 2, 7},
	4:  {0, 3, 8},
	5:  {0, 3, 8},
	6:  {0, 3, 9},
	7:  {0, 3, 9},
	8:  {1, 4, 10},
	9:  {1, 4, 10},
	10: {1, 4, 11},
	11: {1, 4, 11},
	12: {1, 5, 12},
	13: {1, 5, 12},
	14: {1, 5, 13},
	15: {1, 5, 13},
}

func getGID() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}
