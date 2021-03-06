package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/gops/agent"
	"github.com/lionkov/go9p/p"
	"github.com/lionkov/go9p/p/srv"
	"github.com/nicolagi/muscle/config"
	"github.com/nicolagi/muscle/internal/block"
	"github.com/nicolagi/muscle/internal/p9util"
	"github.com/nicolagi/muscle/netutil"
	"github.com/nicolagi/muscle/storage"
	"github.com/nicolagi/muscle/tree"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

var (
	unsupportedModes = map[uint32]error{
		p.DMAPPEND:    fmt.Errorf("append-only files are not supported"),
		p.DMMOUNT:     fmt.Errorf("mounted channels are not supported"),
		p.DMAUTH:      fmt.Errorf("authentication files are not supported"),
		p.DMTMP:       fmt.Errorf("temporary files are not supported"),
		p.DMSYMLINK:   fmt.Errorf("symbolic links are not supported"),
		p.DMLINK:      fmt.Errorf("hard links are not supported"),
		p.DMDEVICE:    fmt.Errorf("device files are not supported"),
		p.DMNAMEDPIPE: fmt.Errorf("named pipes are not supported"),
		p.DMSOCKET:    fmt.Errorf("sockets are not supported"),
		p.DMSETUID:    fmt.Errorf("setuid files are not supported"),
		p.DMSETGID:    fmt.Errorf("setgid files are not supported"),
	}
	knownModes uint32
)

func init() {
	knownModes = 0777 | p.DMDIR | p.DMEXCL
	for mode := range unsupportedModes {
		knownModes |= mode
	}
}

func checkMode(node *tree.Node, mode uint32) error {
	if node != nil {
		if node.IsDir() && mode&p.DMDIR == 0 {
			return fmt.Errorf("a directory cannot become a regular file")
		}
		if !node.IsDir() && mode&p.DMDIR != 0 {
			return fmt.Errorf("a regular file cannot become a directory")
		}
	}
	for bit, err := range unsupportedModes {
		if mode&bit != 0 {
			return err
		}
	}
	if extra := mode &^ knownModes; extra != 0 {
		return fmt.Errorf("unrecognized mode bits: %b", extra)
	}
	return nil
}

type fsNode struct {
	*tree.Node

	dirb p9util.DirBuffer
	lock *nodeLock // Only meaningful for DMEXCL files.
}

func (node *fsNode) prepareForReads() {
	node.dirb.Reset()
	var dir p.Dir
	for _, child := range node.Children() {
		p9util.NodeDirVar(child, &dir)
		node.dirb.Write(&dir)
	}
}

type ops struct {
	treeStore *tree.Store

	// Serializes access to the tree.
	mu   sync.Mutex
	tree *tree.Tree

	// Control node
	c *ctl

	cfg *config.C
}

var (
	_ srv.ReqOps = (*ops)(nil)
	_ srv.FidOps = (*ops)(nil)

	Eunlinked error = &p.Error{Err: "fid points to unlinked node", Errornum: p.EINVAL}
)

func (ops *ops) FidDestroy(fid *srv.Fid) {
	if fid.Aux == nil || fid.Aux == ops.c {
		return
	}
	node := fid.Aux.(*fsNode)
	node.Unref("FidDestroy")
	if node.lock != nil {
		unlockNode(node.lock)
		node.lock = nil
	}
}

func (ops *ops) Attach(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	root := ops.tree.Attach()
	root.Ref("Attach")
	r.Fid.Aux = &fsNode{Node: root}
	qid := p9util.NodeQID(root)
	r.RespondRattach(&qid)
}

func (ops *ops) Walk(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		if len(r.Tc.Wname) == 0 {
			r.Newfid.Aux = ops.c
			r.RespondRwalk(nil)
		} else {
			r.RespondError(srv.Eperm)
		}
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		if len(r.Tc.Wname) == 0 {
			node.Ref("clone")
			r.Newfid.Aux = node
			r.RespondRwalk(nil)
			return
		}
		if node.IsRoot() && len(r.Tc.Wname) == 1 && r.Tc.Wname[0] == "ctl" {
			r.Newfid.Aux = ops.c
			r.RespondRwalk([]p.Qid{ops.c.D.Qid})
			return
		}
		// TODO test scenario: nwqids != 0 but < nwname
		nodes, err := ops.tree.Walk(node.Node, r.Tc.Wname...)
		if errors.Is(err, tree.ErrNotExist) {
			if len(nodes) == 0 {
				r.RespondError(srv.Enoent)
				return
			}
			// Clear the error if it was of type "not found" and we could walk at least a node.
			err = nil
		}
		if err != nil {
			log.WithFields(log.Fields{
				"path":  node.Path(),
				"cause": err.Error(),
			}).Error("Could not walk")
			r.RespondError(srv.Eperm)
			return
		}
		var qids []p.Qid
		for _, n := range nodes {
			qids = append(qids, p9util.NodeQID(n))
		}
		if len(qids) == len(r.Tc.Wname) {
			targetNode := nodes[len(nodes)-1]
			r.Newfid.Aux = &fsNode{Node: targetNode}
			targetNode.Ref("successful walk")
		}
		r.RespondRwalk(qids)
	}
}

func (ops *ops) Open(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if r.Tc.Mode&p.ORCLOSE != 0 {
		r.RespondError(srv.Eperm)
	}
	switch {
	case r.Fid.Aux == ops.c:
		r.RespondRopen(&ops.c.D.Qid, 0)
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		qid := p9util.NodeQID(node.Node)
		if m := moreMode(qid.Path); m&p.DMEXCL != 0 {
			node.lock = lockNode(r.Fid, node.Node)
			if node.lock == nil {
				r.RespondError("file already locked")
				return
			}
			qid.Type |= p.QTEXCL
		}
		switch {
		case node.IsDir():
			if err := ops.tree.Grow(node.Node); err != nil {
				r.RespondError(err)
				return
			}
			node.prepareForReads()
		default:
			if r.Tc.Mode&p.OTRUNC != 0 {
				if err := node.Truncate(0); err != nil {
					r.RespondError(err)
					return
				}
			}
		}
		r.RespondRopen(&qid, 0)
	}
}

func (ops *ops) Create(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		r.RespondError(srv.Eperm)
	default:
		parent := r.Fid.Aux.(*fsNode)
		if parent.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		if err := checkMode(nil, r.Tc.Perm); err != nil {
			r.RespondError(err)
			return
		}
		node, err := ops.tree.Add(parent.Node, r.Tc.Name, r.Tc.Perm)
		if err != nil {
			r.RespondError(err)
			return
		}
		node.Ref("create")
		parent.Unref("created child")
		child := &fsNode{Node: node}
		r.Fid.Aux = child
		qid := p9util.NodeQID(node)
		if r.Tc.Perm&p.DMEXCL != 0 {
			setMoreMode(qid.Path, p.DMEXCL)
			child.lock = lockNode(r.Fid, child.Node)
			if child.lock == nil {
				r.RespondError("out of locks")
				return
			}
			qid.Type |= p.QTEXCL
		}
		r.RespondRcreate(&qid, 0)
	}
}

func (ops *ops) Read(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if err := p.InitRread(r.Rc, r.Tc.Count); err != nil {
		r.RespondError(err)
		return
	}
	switch {
	case r.Fid.Aux == ops.c:
		ops.c.D.Atime = uint32(time.Now().Unix())
		count := ops.c.read(r.Rc.Data[:r.Tc.Count], int(r.Tc.Offset))
		p.SetRreadCount(r.Rc, uint32(count))
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		var count int
		var err error
		if node.IsDir() {
			count, err = node.dirb.Read(r.Rc.Data[:r.Tc.Count], int(r.Tc.Offset))
		} else {
			count, err = node.ReadAt(r.Rc.Data[:r.Tc.Count], int64(r.Tc.Offset))
		}
		if err != nil {
			log.WithFields(log.Fields{
				"path":  node.Path(),
				"cause": err.Error(),
			}).Error("Could not read")
			r.RespondError(srv.Eperm)
			return
		}
		p.SetRreadCount(r.Rc, uint32(count))
	}
	r.Respond()
}

func runCommand(ops *ops, cmd string) error {
	args := strings.Fields(cmd)
	if len(args) == 0 {
		return nil
	}
	cmd = args[0]
	args = args[1:]

	outputBuffer := bytes.NewBuffer(nil)

	// A helper function to return an error, and also add it to the output.
	output := func(err error) error {
		_, _ = fmt.Fprintf(outputBuffer, "%+v", err)
		return err
	}

	// Ensure the output is available even in the case of an early error return.
	defer func() {
		ops.c.contents = outputBuffer.Bytes()
		ops.c.D.Length = uint64(len(ops.c.contents))
	}()

	switch cmd {
	case "diff":
		return doDiff(outputBuffer, ops.tree, ops.treeStore, args)
	case "level":
		if err := setLevel(args[0]); err != nil {
			return err
		}
	case "lsof":
		paths := ops.tree.ListNodesInUse()
		sort.Strings(paths)
		for _, path := range paths {
			outputBuffer.WriteString(path)
			outputBuffer.WriteByte(10)
		}
	case "dump":
		ops.tree.DumpNodes()
	case "keep-local-for":
		parts := strings.SplitN(args[0], "/", 2)
		ops.tree.Ignore(parts[0], parts[1])
		return nil
	case "rename":
		err := ops.tree.Rename(args[0], args[1])
		if err != nil {
			return fmt.Errorf("could not rename %q to %q: %v", args[0], args[1], err)
		}
	case "unlink":
		if len(args) == 0 {
			return errors.New("missing argument to unlink")
		}
		elems := strings.Split(args[0], "/")
		if len(elems) == 0 {
			return errors.Errorf("not enough elements in path: %v", args[0])
		}
		_, r := ops.tree.Root()
		nn, err := ops.tree.Walk(r, elems...)
		if err != nil {
			return errors.Wrapf(err, "could not walk the local tree along %v", elems)
		}
		if len(nn) != len(elems) {
			return errors.Errorf("walked %d path elements, required %d", len(nn), len(elems))
		}
		return ops.tree.RemoveForMerge(nn[len(nn)-1])
	case "graft":
		parts := strings.Split(args[0], "/")
		revision := parts[0]
		historicalPath := parts[1:]
		localPath := strings.Split(args[1], "/")
		localBaseName := localPath[len(localPath)-1]
		localPath = localPath[:len(localPath)-1]
		_, _ = fmt.Fprintf(outputBuffer, "Grafting the node identified by the path elements %v from the revision %q into the local tree by walking the path elements %v\n",
			historicalPath, revision, localPath)
		key, err := storage.NewPointerFromHex(revision)
		if err != nil {
			return fmt.Errorf("%q: %w", revision, err)
		}
		historicalTree, err := tree.NewTree(ops.treeStore, tree.WithRevision(key))
		if err != nil {
			return fmt.Errorf("could not load tree %q: %v", revision, err)
		}
		historicalRoot := historicalTree.Attach()
		hNodes, err := historicalTree.Walk(historicalRoot, historicalPath...)
		if err != nil {
			return fmt.Errorf("could not walk tree %q along nodes %v: %v", revision, historicalPath, err)
		}
		_, localRoot := ops.tree.Root()
		lNodes, err := ops.tree.Walk(localRoot, localPath...)
		if err != nil {
			return fmt.Errorf("could not walk the local tree along %v: %v", localPath, err)
		}
		// TODO also check lnodes is all the names
		localParent := localRoot
		if len(lNodes) > 0 {
			localParent = lNodes[len(lNodes)-1]
		}
		historicalChild := hNodes[len(hNodes)-1]

		fmt.Printf("Attempting graft of %s into %s\n", historicalChild, localParent)
		err = ops.tree.Graft(localParent, historicalChild, localBaseName)
		if err != nil {
			log.WithFields(log.Fields{
				"receiver": localParent,
				"donor":    historicalChild,
				"cause":    err,
			}).Error("Graft failed")
			return srv.Eperm
		}
	case "trim":
		_, root := ops.tree.Root()
		root.Trim()
	case "flush":
		if err := ops.tree.Flush(); err != nil {
			return fmt.Errorf("could not flush: %v", err)
		}
		_, _ = fmt.Fprintln(outputBuffer, "flushed")
	case "pull":
		localbase, err := ops.treeStore.LocalBasePointer()
		if err != nil {
			return output(err)
		}
		remotebase, err := ops.treeStore.RemoteBasePointer()
		if err != nil {
			return output(err)
		}
		if localbase.Equals(remotebase) {
			_, _ = fmt.Fprintln(outputBuffer, "local base matches remote base, pull is a no-op")
			return nil
		}
		localbasetree, err := tree.NewTree(ops.treeStore, tree.WithRevision(localbase))
		if err != nil {
			return output(err)
		}
		remotebasetree, err := tree.NewTree(ops.treeStore, tree.WithRevision(remotebase))
		if err != nil {
			return output(err)
		}
		commands, err := ops.tree.PullWorklog(ops.cfg, localbasetree, remotebasetree)
		if err != nil {
			return output(err)
		}
		if len(commands) == 0 {
			_, _ = fmt.Fprintln(outputBuffer, "no commands to run, pull is a no-op")
			if err := ops.treeStore.SetLocalBasePointer(remotebase); err != nil {
				return output(err)
			}
			return nil
		}
		outputBuffer.WriteString(commands)
		return nil
	case "push":
		localbase, err := ops.treeStore.LocalBasePointer()
		if err != nil {
			return output(err)
		}
		remotebase, err := ops.treeStore.RemoteBasePointer()
		if err != nil {
			return output(err)
		}
		if !localbase.Equals(remotebase) {
			return output(errors.Errorf("local base %v does not match remote base %v, pull first", localbase, remotebase))
		}
		_, _ = fmt.Fprintln(outputBuffer, "local base matches remote base, push allowed")

		if err := ops.tree.Flush(); err != nil {
			return output(err)
		}
		_, _ = fmt.Fprintln(outputBuffer, "push: flushed")

		if err := ops.tree.Seal(); err != nil {
			return output(err)
		}
		_, _ = fmt.Fprintln(outputBuffer, "push: sealed")

		_, localroot := ops.tree.Root()
		revision := tree.NewRevision(localroot, remotebase)
		if err := ops.treeStore.StoreRevision(revision); err != nil {
			return output(err)
		}
		ops.tree.SetRevision(revision)
		_, _ = fmt.Fprintf(outputBuffer, "push: revision created: %s\n", revision.ShortString())

		if err := ops.treeStore.SetRemoteBasePointer(revision.Key()); err != nil {
			return output(err)
		}
		_, _ = fmt.Fprintf(outputBuffer, "push: updated remote base pointer: %v\n", revision.Key())
		if err := ops.treeStore.SetLocalBasePointer(revision.Key()); err != nil {
			return output(err)
		}
		_, _ = fmt.Fprintf(outputBuffer, "push: updated local base pointer: %v\n", revision.Key())
		return nil
	default:
		return fmt.Errorf("command not recognized: %q", cmd)
	}

	return nil
}

func (ops *ops) Write(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		ops.c.D.Mtime = uint32(time.Now().Unix())
		// Assumption: One Twrite per command.
		if err := runCommand(ops, string(r.Tc.Data)); err != nil {
			r.RespondError(err)
			return
		}
		r.RespondRwrite(uint32(len(r.Tc.Data)))
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		if err := node.WriteAt(r.Tc.Data, int64(r.Tc.Offset)); err != nil {
			r.RespondError(err)
			return
		}
		r.RespondRwrite(uint32(len(r.Tc.Data)))
	}
}

func (ops *ops) Clunk(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if r.Fid.Aux != ops.c {
		node := r.Fid.Aux.(*fsNode)
		if node.lock != nil {
			unlockNode(node.lock)
			node.lock = nil
		}
	}
	r.RespondRclunk()
}

func (ops *ops) Remove(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		r.RespondError(srv.Eperm)
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		err := ops.tree.Remove(node.Node)
		if err != nil {
			if errors.Is(err, tree.ErrNotEmpty) {
				r.RespondError(srv.Enotempty)
			} else {
				log.Printf("%s: %+v", node.Path(), err)
				r.RespondError(srv.Eperm)
			}
		} else {
			r.RespondRremove()
		}
	}
}

func (ops *ops) Stat(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		r.RespondRstat(&ops.c.D)
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		dir := p9util.NodeDir(node.Node)
		if m := moreMode(dir.Qid.Path); m&p.DMEXCL != 0 {
			dir.Mode |= p.DMEXCL
			dir.Qid.Type |= p.QTEXCL
		} else {
			dir.Mode &^= p.DMEXCL
			dir.Qid.Type &^= p.QTEXCL
		}
		r.RespondRstat(&dir)
	}
}

func (ops *ops) Wstat(r *srv.Req) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	switch {
	case r.Fid.Aux == ops.c:
		r.RespondError(srv.Eperm)
	default:
		node := r.Fid.Aux.(*fsNode)
		if node.Unlinked() {
			r.RespondError(Eunlinked)
			return
		}
		dir := r.Tc.Dir
		if dir.ChangeLength() {
			if node.IsDir() {
				r.RespondError(srv.Eperm)
				return
			}
			if err := node.Truncate(dir.Length); err != nil {
				log.WithFields(log.Fields{
					"cause": err,
				}).Error("Could not truncate")
				r.RespondError(srv.Eperm)
				return
			}
		}

		// From the documentation: "ChangeIllegalFields returns true
		// if Dir contains values that would request illegal fields to
		// be changed; these are type, dev, and qid. The size field is
		// ignored because it's not kept intact.  Any 9p server should
		// return error to a Wstat request when this method returns true."
		// Unfortunately (at least mounting on Linux using the 9p module)
		// rename operations issue Wstat calls with non-empty muid.
		// Since we need renames to work, let's just discard the muid
		// if present. Also, Linux tries to set the atime.  In order not to
		// fail commands such as touch, we'll ignore those also.
		dir.Atime = ^uint32(0)
		dir.Muid = ""
		if dir.ChangeIllegalFields() {
			log.WithFields(log.Fields{
				"path": node.Path(),
				"dir":  dir,
				"qid":  dir.Qid,
			}).Warning("Trying to change illegal fields")
			r.RespondError(srv.Eperm)
			return
		}

		if dir.ChangeName() {
			node.Rename(dir.Name)
		}
		if dir.ChangeMtime() {
			node.Touch(dir.Mtime)
		}

		if dir.ChangeMode() {
			if err := checkMode(node.Node, dir.Mode); err != nil {
				r.RespondError(err)
				return
			}
			qid := p9util.NodeQID(node.Node)
			if dir.Mode&p.DMEXCL != 0 {
				setMoreMode(qid.Path, p.DMEXCL)
			} else {
				setMoreMode(qid.Path, 0)
			}
			node.SetPerm(dir.Mode & 0777)
		}

		// TODO: Not sure it's best to 'pretend' it works, or fail.
		if dir.ChangeGID() {
			r.RespondError(srv.Eperm)
			return
		}

		r.RespondRwstat()
	}
}

func setLevel(level string) error {
	ll, err := log.ParseLevel(level)
	if err != nil {
		return err
	}
	log.SetLevel(ll)
	return nil
}

func main() {
	if err := agent.Listen(agent.Options{
		ShutdownCleanup: true,
	}); err != nil {
		log.Printf("Could not start gops agent: %v", err)
	}

	base := flag.String("base", config.DefaultBaseDirectoryPath, "Base directory for configuration, logs and cache files")
	flag.Parse()
	cfg, err := config.Load(*base)
	if err != nil {
		log.Fatalf("Could not load config from %q: %v", *base, err)
	}
	log.SetFormatter(&log.JSONFormatter{})

	remoteBasicStore, err := storage.NewStore(cfg)
	if err != nil {
		log.Fatalf("Could not create remote store: %v", err)
	}

	stagingStore := storage.NewDiskStore(cfg.StagingDirectoryPath())
	cacheStore := storage.NewDiskStore(cfg.CacheDirectoryPath())
	pairedStore, err := storage.NewPaired(cacheStore, remoteBasicStore, cfg.PropagationLogFilePath())
	if err != nil {
		log.Fatalf("Could not start new paired store with log %q: %v", cfg.PropagationLogFilePath(), err)
	}

	// The paired store starts propagation of blocks from the local to
	// the remote store on the first put operation.  which happens when
	// taking a snapshot (at that time, data moves from the staging area
	// to the paired store, which then propagates from local to remote
	// asynchronously). Since there may be still blocks to propagate
	// from a previous run (i.e., musclefs was killed before all blocks
	// were copied to the remote store) we need to start the background
	// propagation immediately.
	pairedStore.EnsureBackgroundPuts()

	blockFactory, err := block.NewFactory(stagingStore, pairedStore, cfg.EncryptionKeyBytes())
	if err != nil {
		log.Fatalf("Could not build block factory: %v", err)
	}
	treeStore, err := tree.NewStore(blockFactory, remoteBasicStore, *base)
	if err != nil {
		log.Fatalf("Could not load tree: %v", err)
	}
	rootKey, err := treeStore.LocalRootKey()
	if err != nil {
		log.Fatalf("Could not load tree: %v", err)
	}
	tt, err := tree.NewTree(treeStore, tree.WithRoot(rootKey), tree.WithMutable(cfg.BlockSize))
	if err != nil {
		log.Fatalf("Could not load tree: %v", err)
	}

	ops := &ops{
		treeStore: treeStore,
		tree:      tt,
		c:         new(ctl),
		cfg:       cfg,
	}

	_, root := tt.Root()
	now := time.Now()
	ops.c.D.Qid.Path = uint64(now.UnixNano())
	ops.c.D.Mode = 0644
	ops.c.D.Mtime = uint32(now.Unix())
	ops.c.D.Atime = ops.c.D.Mtime
	ops.c.D.Name = "ctl"
	ops.c.D.Uid = p9util.NodeUID
	ops.c.D.Gid = p9util.NodeGID

	/* Best-effort clean-up, for when the control file used to be part of the tree. */
	if nodes, err := ops.tree.Walk(root, "ctl"); err == nil && len(nodes) == 1 {
		_ = ops.tree.Remove(nodes[0])
	}

	fs := &srv.Srv{}
	fs.Dotu = false
	fs.Id = "muscle"
	if !fs.Start(ops) {
		log.Fatal("go9p/p/srv.Srv.Start returned false")
	}

	go func() {
		if listener, err := netutil.Listen(cfg.ListenNet, cfg.ListenAddr); err != nil {
			log.Fatalf("Could not start net listener: %v", err)
		} else if err := fs.StartListener(listener); err != nil {
			log.Fatalf("Could not start 9P listener: %v", err)
		}
	}()

	// need to be flushed to the disk cache.
	go func() {
		for {
			time.Sleep(tree.SnapshotFrequency)
			ops.mu.Lock()
			// TODO handle all errors - add errcheck to precommit?
			if err := ops.tree.FlushIfNotDoneRecently(); err != nil {
				log.Printf("Could not flush: %v", err)
			}
			ops.mu.Unlock()
		}
	}()

	// Now just wait for a signal to do the clean-up.
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	for range c {
		log.Info("Final clean-up")
		ops.mu.Lock()
		if err := tt.Flush(); err != nil {
			log.Printf("Could not flush: %v", err)
			ops.mu.Unlock()
			continue
		}
		ops.mu.Unlock()
		break
	}
}
