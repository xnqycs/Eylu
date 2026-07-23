package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"
)

const (
	defaultCodeContextCacheBytes = 64 << 20
	defaultMaxReadLines          = 2000
	defaultCodeIndexWorkers      = 4
)

type CodeContextOptions struct {
	MaxCacheBytes   int64
	MaxReadLines    int
	IndexWorkers    int
	RefreshInterval time.Duration
}

type CodeSlice struct {
	Content       string
	RelativePath  string
	AbsolutePath  string
	StartLine     int
	EndLine       int
	TotalLines    int
	FileHash      string
	SliceHash     string
	ArtifactID    string
	NextStartLine int
	Truncated     bool
	Bytes         int64
	CacheHit      bool
}

type cachedCodeFile struct {
	file       IndexedFile
	data       []byte
	lineStarts []int
	totalLines int
	fileHash   string
	binary     bool
	lastUsed   uint64
}

// CodeContext owns the session-level repository manifest, immutable file
// snapshots, line offsets, hashes, LRU content cache, and lexical postings.
type CodeContext struct {
	index           *RepositoryIndex
	maxCacheBytes   int64
	maxReadLines    int
	indexWorkers    int
	refreshInterval time.Duration

	refreshMu            sync.Mutex
	lexicalMu            sync.Mutex
	loads                singleflight.Group
	readFile             func(string) ([]byte, error)
	mu                   sync.RWMutex
	initialized          bool
	lastRefresh          time.Time
	generation           uint64
	snapshot             IndexSnapshot
	cache                map[string]*cachedCodeFile
	cacheBytes           int64
	clock                uint64
	lexicalGeneration    uint64
	lexicalMaxFileBytes  int64
	lexicalSkippedBinary int
	lexicalSkippedLarge  int
	trigrams             map[uint32][]uint32
}

func NewCodeContext(workspace string, options CodeContextOptions) (*CodeContext, error) {
	index, err := NewRepositoryIndex(workspace)
	if err != nil {
		return nil, err
	}
	return newCodeContext(index, options), nil
}

func newCodeContext(index *RepositoryIndex, options CodeContextOptions) *CodeContext {
	if options.MaxCacheBytes <= 0 {
		options.MaxCacheBytes = defaultCodeContextCacheBytes
	}
	if options.MaxReadLines <= 0 {
		options.MaxReadLines = defaultMaxReadLines
	}
	if options.IndexWorkers <= 0 {
		options.IndexWorkers = defaultCodeIndexWorkers
	}
	if options.RefreshInterval <= 0 {
		options.RefreshInterval = time.Second
	}
	return &CodeContext{
		index: index, maxCacheBytes: options.MaxCacheBytes, maxReadLines: options.MaxReadLines,
		indexWorkers: options.IndexWorkers, refreshInterval: options.RefreshInterval,
		readFile: os.ReadFile, cache: make(map[string]*cachedCodeFile), trigrams: make(map[uint32][]uint32),
	}
}

func (c *CodeContext) RepositoryIndex() *RepositoryIndex { return c.index }

func (c *CodeContext) MaxReadLines() int { return c.maxReadLines }

func (c *CodeContext) Refresh(ctx context.Context) (IndexSnapshot, uint64) {
	if c == nil || c.index == nil {
		return IndexSnapshot{}, 0
	}
	c.mu.RLock()
	if c.initialized && time.Since(c.lastRefresh) < c.refreshInterval {
		snapshot, generation := cloneSnapshot(c.snapshot), c.generation
		c.mu.RUnlock()
		return snapshot, generation
	}
	c.mu.RUnlock()

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.RLock()
	if c.initialized && time.Since(c.lastRefresh) < c.refreshInterval {
		snapshot, generation := cloneSnapshot(c.snapshot), c.generation
		c.mu.RUnlock()
		return snapshot, generation
	}
	c.mu.RUnlock()

	snapshot := c.index.Refresh(ctx)
	c.mu.Lock()
	if !c.initialized || !sameIndexManifest(c.snapshot.Files, snapshot.Files) {
		c.generation++
		c.lexicalGeneration = 0
		c.lexicalMaxFileBytes = 0
		c.trigrams = make(map[uint32][]uint32)
		c.dropStaleCacheLocked(snapshot.Files)
	}
	c.snapshot = cloneSnapshot(snapshot)
	c.initialized = true
	c.lastRefresh = time.Now()
	generation := c.generation
	c.mu.Unlock()
	return snapshot, generation
}

func (c *CodeContext) Invalidate(path string) {
	if c == nil {
		return
	}
	absolute := path
	if c.index != nil && !filepath.IsAbs(path) {
		if resolved, err := c.index.paths.resourcePath(path); err == nil {
			absolute = resolved
		}
	}
	c.mu.Lock()
	if cached := c.cache[absolute]; cached != nil {
		c.cacheBytes -= int64(len(cached.data))
		delete(c.cache, absolute)
	}
	c.generation++
	c.lexicalGeneration = 0
	c.lexicalMaxFileBytes = 0
	c.trigrams = make(map[uint32][]uint32)
	c.lastRefresh = time.Time{}
	c.mu.Unlock()
}

func (c *CodeContext) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.generation++
	c.lexicalGeneration = 0
	c.lexicalMaxFileBytes = 0
	c.trigrams = make(map[uint32][]uint32)
	c.lastRefresh = time.Time{}
	c.mu.Unlock()
}

func (c *CodeContext) ReadSlice(ctx context.Context, reference string, startLine, endLine, maxBytes int) (CodeSlice, error) {
	if c == nil || c.index == nil {
		return CodeSlice{}, errors.New("code context is unavailable")
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine > 0 && endLine < startLine {
		return CodeSlice{}, errors.New("end_line must be greater than or equal to start_line")
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	absolute, err := c.index.paths.existing(reference)
	if err != nil {
		return CodeSlice{}, err
	}
	file, cacheHit, err := c.loadFile(ctx, absolute)
	if err != nil {
		return CodeSlice{}, err
	}
	if file.binary {
		return CodeSlice{}, errors.New("file is not valid UTF-8 text")
	}
	if file.totalLines == 0 {
		if startLine != 1 {
			return CodeSlice{}, errors.New("start_line exceeds file length")
		}
		return c.makeSlice(file, nil, 1, 0, 0, false, cacheHit), nil
	}
	if startLine > file.totalLines {
		return CodeSlice{}, errors.New("start_line exceeds file length")
	}
	requestedEnd := endLine
	if requestedEnd <= 0 || requestedEnd > file.totalLines {
		requestedEnd = file.totalLines
	}
	actualEnd := requestedEnd
	truncated := false
	if actualEnd-startLine+1 > c.maxReadLines {
		actualEnd = startLine + c.maxReadLines - 1
		truncated = true
	}
	startByte := file.lineStarts[startLine-1]
	endByte := len(file.data)
	if actualEnd < file.totalLines {
		endByte = file.lineStarts[actualEnd]
	}
	if endByte-startByte > maxBytes {
		limit := startByte + maxBytes
		for limit > startByte && !utf8.RuneStart(file.data[limit]) {
			limit--
		}
		if newline := bytes.LastIndexByte(file.data[startByte:limit], '\n'); newline >= 0 {
			limit = startByte + newline + 1
		}
		if limit == startByte {
			limit = startByte + maxBytes
			for limit > startByte && !utf8.RuneStart(file.data[limit]) {
				limit--
			}
		}
		endByte = limit
		newlineCount := bytes.Count(file.data[startByte:endByte], []byte{'\n'})
		actualEnd = startLine + newlineCount
		if endByte > startByte && file.data[endByte-1] == '\n' {
			actualEnd--
		}
		if actualEnd > file.totalLines {
			actualEnd = file.totalLines
		}
		truncated = true
	}
	return c.makeSlice(file, file.data[startByte:endByte], startLine, actualEnd, endByte-startByte, truncated, cacheHit), nil
}

func (c *CodeContext) makeSlice(file *cachedCodeFile, data []byte, startLine, endLine, byteCount int, truncated, cacheHit bool) CodeSlice {
	sliceDigest := sha256.Sum256(data)
	sliceHash := hex.EncodeToString(sliceDigest[:])
	next := 0
	if truncated && endLine < file.totalLines {
		next = endLine + 1
	}
	artifactDigest := sha256.Sum256([]byte(file.file.Relative + "\x00" + file.fileHash + "\x00" + fmt.Sprintf("%d:%d", startLine, endLine) + "\x00" + sliceHash))
	return CodeSlice{
		Content: string(data), RelativePath: file.file.Relative, AbsolutePath: file.file.Absolute,
		StartLine: startLine, EndLine: endLine, TotalLines: file.totalLines, FileHash: file.fileHash,
		SliceHash: sliceHash, ArtifactID: "code:" + hex.EncodeToString(artifactDigest[:16]),
		NextStartLine: next, Truncated: truncated, Bytes: file.file.Size, CacheHit: cacheHit,
	}
}

func (c *CodeContext) loadFile(ctx context.Context, absolute string) (*cachedCodeFile, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("path is not a regular file")
	}
	c.mu.Lock()
	if cached := c.cache[absolute]; cached != nil && cached.file.Size == info.Size() && cached.file.ModTimeUnixNano == info.ModTime().UnixNano() {
		c.clock++
		cached.lastUsed = c.clock
		c.mu.Unlock()
		return cached, true, nil
	}
	c.mu.Unlock()
	result := c.loads.DoChan(absolute, func() (any, error) {
		return c.loadFileUncached(absolute)
	})
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case loaded := <-result:
		if loaded.Err != nil {
			return nil, false, loaded.Err
		}
		value := loaded.Val.(loadedCodeFile)
		return value.file, value.cacheHit, nil
	}
}

type loadedCodeFile struct {
	file     *cachedCodeFile
	cacheHit bool
}

func (c *CodeContext) loadFileUncached(absolute string) (loadedCodeFile, error) {
	info, err := os.Stat(absolute)
	if err != nil {
		return loadedCodeFile{}, err
	}
	if !info.Mode().IsRegular() {
		return loadedCodeFile{}, errors.New("path is not a regular file")
	}
	c.mu.Lock()
	if cached := c.cache[absolute]; cached != nil && cached.file.Size == info.Size() && cached.file.ModTimeUnixNano == info.ModTime().UnixNano() {
		c.clock++
		cached.lastUsed = c.clock
		c.mu.Unlock()
		return loadedCodeFile{file: cached, cacheHit: true}, nil
	}
	c.mu.Unlock()

	data, err := c.readFile(absolute)
	if err != nil {
		return loadedCodeFile{}, err
	}
	relative, err := c.index.indexedFile(absolute)
	if err != nil {
		return loadedCodeFile{}, err
	}
	digest := sha256.Sum256(data)
	lineStarts := []int{0}
	for index, value := range data {
		if value == '\n' {
			lineStarts = append(lineStarts, index+1)
		}
	}
	totalLines := len(lineStarts)
	if len(data) == 0 {
		totalLines = 0
		lineStarts = nil
	}
	cached := &cachedCodeFile{
		file: relative, data: data, lineStarts: lineStarts, totalLines: totalLines,
		fileHash: hex.EncodeToString(digest[:]), binary: bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data),
	}
	c.mu.Lock()
	c.clock++
	cached.lastUsed = c.clock
	if previous := c.cache[absolute]; previous != nil {
		c.cacheBytes -= int64(len(previous.data))
	}
	if int64(len(data)) <= c.maxCacheBytes {
		c.cache[absolute] = cached
		c.cacheBytes += int64(len(data))
		c.evictLocked(absolute)
	}
	c.mu.Unlock()
	return loadedCodeFile{file: cached}, nil
}

func (c *CodeContext) CandidateFiles(ctx context.Context, query string, regexPrefix string, maxFileBytes int64) (IndexSnapshot, uint64, []IndexedFile, error) {
	snapshot, generation := c.Refresh(ctx)
	if err := c.ensureLexicalIndex(ctx, snapshot, generation, maxFileBytes); err != nil {
		return snapshot, generation, nil, err
	}
	needle := query
	if regexPrefix != "" {
		needle = regexPrefix
	}
	if len(needle) < 3 {
		return snapshot, generation, snapshot.Files, nil
	}
	grams := uniqueTrigrams([]byte(needle))
	c.mu.RLock()
	var fileIDs []uint32
	for index, gram := range grams {
		postings := c.trigrams[gram]
		if index == 0 {
			fileIDs = append([]uint32(nil), postings...)
		} else {
			fileIDs = intersectSortedFileIDs(fileIDs, postings)
		}
		if len(fileIDs) == 0 {
			break
		}
	}
	c.mu.RUnlock()
	files := make([]IndexedFile, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		if int(fileID) < len(snapshot.Files) {
			files = append(files, snapshot.Files[fileID])
		}
	}
	return snapshot, generation, files, nil
}

func (c *CodeContext) LexicalStats(generation uint64, maxFileBytes int64) (skippedBinary, skippedLarge int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lexicalGeneration != generation || c.lexicalMaxFileBytes != maxFileBytes {
		return 0, 0
	}
	return c.lexicalSkippedBinary, c.lexicalSkippedLarge
}

func (c *CodeContext) FileText(ctx context.Context, file IndexedFile) ([]byte, string, bool, bool, error) {
	cached, hit, err := c.loadFile(ctx, file.Absolute)
	if err != nil {
		return nil, "", false, hit, err
	}
	return cached.data, cached.fileHash, cached.binary, hit, nil
}

func (c *CodeContext) ensureLexicalIndex(ctx context.Context, snapshot IndexSnapshot, generation uint64, maxFileBytes int64) error {
	c.mu.RLock()
	ready := c.lexicalGeneration == generation && c.lexicalMaxFileBytes == maxFileBytes
	c.mu.RUnlock()
	if ready {
		return nil
	}
	c.lexicalMu.Lock()
	defer c.lexicalMu.Unlock()
	c.mu.RLock()
	ready = c.lexicalGeneration == generation && c.lexicalMaxFileBytes == maxFileBytes
	c.mu.RUnlock()
	if ready {
		return nil
	}

	type indexedGrams struct {
		fileID uint32
		grams  []uint32
		binary bool
	}
	type indexJob struct {
		fileID uint32
		file   IndexedFile
	}
	jobs := make(chan indexJob)
	var workers sync.WaitGroup
	workerCount := c.indexWorkers
	if workerCount > len(snapshot.Files) {
		workerCount = len(snapshot.Files)
	}
	resultBuffer := workerCount
	if resultBuffer == 0 {
		resultBuffer = 1
	}
	results := make(chan indexedGrams, resultBuffer)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				if job.file.Size > maxFileBytes || ctx.Err() != nil {
					continue
				}
				data, _, binary, _, err := c.FileText(ctx, job.file)
				if err == nil {
					grams := []uint32(nil)
					if !binary {
						grams = uniqueTrigrams(data)
					}
					select {
					case results <- indexedGrams{fileID: job.fileID, grams: grams, binary: binary}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index, file := range snapshot.Files {
			select {
			case jobs <- indexJob{fileID: uint32(index), file: file}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	postings := make(map[uint32][]uint32)
	skippedBinary := 0
	for result := range results {
		if result.binary {
			skippedBinary++
			continue
		}
		for _, gram := range result.grams {
			postings[gram] = append(postings[gram], result.fileID)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for gram := range postings {
		sort.Slice(postings[gram], func(i, j int) bool { return postings[gram][i] < postings[gram][j] })
	}
	c.mu.Lock()
	if c.generation == generation {
		c.trigrams = postings
		c.lexicalGeneration = generation
		c.lexicalMaxFileBytes = maxFileBytes
		c.lexicalSkippedBinary = skippedBinary
		c.lexicalSkippedLarge = 0
		for _, file := range snapshot.Files {
			if file.Size > maxFileBytes {
				c.lexicalSkippedLarge++
			}
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *CodeContext) evictLocked(keep string) {
	for c.cacheBytes > c.maxCacheBytes {
		oldestPath := ""
		oldestUse := ^uint64(0)
		for path, cached := range c.cache {
			if path != keep && cached.lastUsed < oldestUse {
				oldestPath, oldestUse = path, cached.lastUsed
			}
		}
		if oldestPath == "" {
			break
		}
		c.cacheBytes -= int64(len(c.cache[oldestPath].data))
		delete(c.cache, oldestPath)
	}
}

func (c *CodeContext) dropStaleCacheLocked(files []IndexedFile) {
	manifest := make(map[string]IndexedFile, len(files))
	for _, file := range files {
		manifest[file.Absolute] = file
	}
	for path, cached := range c.cache {
		current, ok := manifest[path]
		if ok && current.Size == cached.file.Size && current.ModTimeUnixNano == cached.file.ModTimeUnixNano {
			continue
		}
		c.cacheBytes -= int64(len(cached.data))
		delete(c.cache, path)
	}
}

func sameIndexManifest(left, right []IndexedFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Relative != right[index].Relative || left[index].Size != right[index].Size || left[index].ModTimeUnixNano != right[index].ModTimeUnixNano {
			return false
		}
	}
	return true
}

func uniqueTrigrams(data []byte) []uint32 {
	if len(data) < 3 {
		return nil
	}
	capacity := len(data)
	if capacity > 64<<10 {
		capacity = 64 << 10
	}
	seen := make(map[uint32]struct{}, capacity)
	for index := 0; index+3 <= len(data); index++ {
		gram := uint32(data[index])<<16 | uint32(data[index+1])<<8 | uint32(data[index+2])
		seen[gram] = struct{}{}
	}
	grams := make([]uint32, 0, len(seen))
	for gram := range seen {
		grams = append(grams, gram)
	}
	sort.Slice(grams, func(i, j int) bool { return grams[i] < grams[j] })
	return grams
}

func intersectSortedFileIDs(left, right []uint32) []uint32 {
	result := make([]uint32, 0)
	for i, j := 0, 0; i < len(left) && j < len(right); {
		switch {
		case left[i] < right[j]:
			i++
		case left[i] > right[j]:
			j++
		case left[i] == right[j]:
			result = append(result, left[i])
			i++
			j++
		}
	}
	return result
}
