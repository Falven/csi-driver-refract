package refract

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type RefractRoot struct {
	*fs.LoopbackRoot
	NewNode func(rootData *RefractRoot, parent *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder
}

func (r *RefractRoot) newNode(parent *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder {
	if r.NewNode != nil {
		return r.NewNode(r, parent, name, st)
	}

	lbNode := &fs.LoopbackNode{
		RootData: r.LoopbackRoot,
	}

	return &RefractNode{
		LoopbackNode: lbNode,
	}
}

// NewRefractRoot returns a root node for a refract file system whose
// root is at the given root. This node implements all NodeXxxxer
// operations available.
func NewRefractRoot(rootPath string) (fs.InodeEmbedder, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(rootPath, &st); err != nil {
		return nil, err
	}

	lbRoot := &fs.LoopbackRoot{
		Path: rootPath,
		Dev:  uint64(st.Dev),
	}

	rfRoot := &RefractRoot{
		LoopbackRoot: lbRoot,
	}

	return rfRoot.newNode(nil, "", &st), nil
}

var _ = (fs.InodeEmbedder)((*RefractNode)(nil))

type RefractNode struct {
	*fs.LoopbackNode
}

var _ = (fs.NodeStatfser)((*RefractNode)(nil))

func (n *RefractNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	return n.LoopbackNode.Statfs(ctx, out)
}

var _ = (fs.NodeLookuper)((*RefractNode)(nil))

func (n *RefractNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.LoopbackNode.Lookup(ctx, name, out)
}

var _ = (fs.NodeMknoder)((*RefractNode)(nil))

func (n *RefractNode) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.LoopbackNode.Mknod(ctx, name, mode, rdev, out)
}

var _ = (fs.NodeMkdirer)((*RefractNode)(nil))

func (n *RefractNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

var _ = (fs.NodeRmdirer)((*RefractNode)(nil))

func (n *RefractNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return n.LoopbackNode.Rmdir(ctx, name)
}

var _ = (fs.NodeUnlinker)((*RefractNode)(nil))

func (n *RefractNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return n.LoopbackNode.Unlink(ctx, name)
}

var _ = (fs.NodeRenamer)((*RefractNode)(nil))

func (n *RefractNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}

var _ = (fs.NodeCreater)((*RefractNode)(nil))

func (n *RefractNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	return n.LoopbackNode.Create(ctx, name, flags, mode, out)
}

var _ = (fs.NodeSymlinker)((*RefractNode)(nil))

func (n *RefractNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.LoopbackNode.Symlink(ctx, target, name, out)
}

var _ = (fs.NodeLinker)((*RefractNode)(nil))

func (n *RefractNode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return n.LoopbackNode.Link(ctx, target, name, out)
}

var _ = (fs.NodeReadlinker)((*RefractNode)(nil))

func (n *RefractNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return n.LoopbackNode.Readlink(ctx)
}

var _ = (fs.NodeOpener)((*RefractNode)(nil))

func (n *RefractNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	return n.LoopbackNode.Open(ctx, flags)
}

var _ = (fs.NodeOpendirer)((*RefractNode)(nil))

func (n *RefractNode) Opendir(ctx context.Context) syscall.Errno {
	return n.LoopbackNode.Opendir(ctx)
}

var _ = (fs.NodeReaddirer)((*RefractNode)(nil))

func (n *RefractNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return n.LoopbackNode.Readdir(ctx)
}

var _ = (fs.NodeGetattrer)((*RefractNode)(nil))

func (n *RefractNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return n.LoopbackNode.Getattr(ctx, f, out)
}

var _ = (fs.NodeSetattrer)((*RefractNode)(nil))

func (n *RefractNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}
