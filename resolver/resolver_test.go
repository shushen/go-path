package resolver_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	bsfetcher "github.com/ipfs/go-fetcher/impl/blockservice"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	dagpb "github.com/ipld/go-codec-dagpb"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/schema"

	merkledag "github.com/ipfs/go-merkledag"
	path "github.com/ipfs/go-path"
	"github.com/ipfs/go-path/resolver"
	"github.com/ipfs/go-unixfsnode"
	_ "github.com/ipld/go-ipld-prime/codec/dagcbor"
	dagcbor "github.com/ipld/go-ipld-prime/codec/dagcbor"
	dagjson "github.com/ipld/go-ipld-prime/codec/dagjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randNode() *merkledag.ProtoNode {
	node := new(merkledag.ProtoNode)
	node.SetData(make([]byte, 32))
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Read(node.Data())
	return node
}

func TestRecurivePathResolution(t *testing.T) {
	ctx := context.Background()
	bsrv := mockBlockService()

	a := randNode()
	b := randNode()
	c := randNode()

	err := b.AddNodeLink("grandchild", c)
	if err != nil {
		t.Fatal(err)
	}

	err = a.AddNodeLink("child", b)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []*merkledag.ProtoNode{a, b, c} {
		err = bsrv.AddBlock(n)
		if err != nil {
			t.Fatal(err)
		}
	}

	aKey := a.Cid()

	segments := []string{aKey.String(), "child", "grandchild"}
	p, err := path.FromSegments("/ipfs/", segments...)
	if err != nil {
		t.Fatal(err)
	}

	fetcherFactory := bsfetcher.NewFetcherConfig(bsrv)
	fetcherFactory.NodeReifier = unixfsnode.Reify
	fetcherFactory.PrototypeChooser = dagpb.AddSupportToChooser(func(lnk ipld.Link, lnkCtx ipld.LinkContext) (ipld.NodePrototype, error) {
		if tlnkNd, ok := lnkCtx.LinkNode.(schema.TypedLinkNode); ok {
			return tlnkNd.LinkTargetNodePrototype(), nil
		}
		return basicnode.Prototype.Any, nil
	})
	resolver := resolver.NewBasicResolver(fetcherFactory)

	node, lnk, err := resolver.ResolvePath(ctx, p)
	if err != nil {
		t.Fatal(err)
	}

	uNode, ok := node.(unixfsnode.PathedPBNode)
	require.True(t, ok)
	fd := uNode.FieldData()
	byts, err := fd.Must().AsBytes()
	require.NoError(t, err)

	assert.Equal(t, cidlink.Link{c.Cid()}, lnk)

	assert.Equal(t, c.Data(), byts)
	cKey := c.Cid()

	rCid, rest, err := resolver.ResolveToLastNode(ctx, p)
	if err != nil {
		t.Fatal(err)
	}

	if len(rest) != 0 {
		t.Error("expected rest to be empty")
	}

	if rCid.String() != cKey.String() {
		t.Fatal(fmt.Errorf(
			"ResolveToLastNode failed for %s: %s != %s",
			p.String(), rCid.String(), cKey.String()))
	}

	p2, err := path.FromSegments("/ipfs/", aKey.String())
	if err != nil {
		t.Fatal(err)
	}

	rCid, rest, err = resolver.ResolveToLastNode(ctx, p2)
	if err != nil {
		t.Fatal(err)
	}

	if len(rest) != 0 {
		t.Error("expected rest to be empty")
	}

	if rCid.String() != aKey.String() {
		t.Fatal(fmt.Errorf(
			"ResolveToLastNode failed for %s: %s != %s",
			p.String(), rCid.String(), cKey.String()))
	}
}

func TestResolveToLastNode_NoUnnecessaryFetching(t *testing.T) {
	ctx := context.Background()
	bsrv := mockBlockService()

	a := randNode()
	b := randNode()

	err := a.AddNodeLink("child", b)
	require.NoError(t, err)

	err = bsrv.AddBlock(a)
	require.NoError(t, err)

	aKey := a.Cid()

	segments := []string{aKey.String(), "child"}
	p, err := path.FromSegments("/ipfs/", segments...)
	require.NoError(t, err)

	fetcherFactory := bsfetcher.NewFetcherConfig(bsrv)
	fetcherFactory.PrototypeChooser = dagpb.AddSupportToChooser(func(lnk ipld.Link, lnkCtx ipld.LinkContext) (ipld.NodePrototype, error) {
		if tlnkNd, ok := lnkCtx.LinkNode.(schema.TypedLinkNode); ok {
			return tlnkNd.LinkTargetNodePrototype(), nil
		}
		return basicnode.Prototype.Any, nil
	})
	fetcherFactory.NodeReifier = unixfsnode.Reify
	resolver := resolver.NewBasicResolver(fetcherFactory)

	resolvedCID, remainingPath, err := resolver.ResolveToLastNode(ctx, p)
	require.NoError(t, err)

	require.Equal(t, len(remainingPath), 0, "cannot have remaining path")
	require.Equal(t, b.Cid(), resolvedCID)
}

func TestPathRemainder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bsrv := mockBlockService()

	nb := basicnode.Prototype.Any.NewBuilder()
	err := dagjson.Decode(nb, strings.NewReader(`{"foo": {"bar": "baz"}}`))
	require.NoError(t, err)
	out := new(bytes.Buffer)
	err = dagcbor.Encode(nb.Build(), out)
	require.NoError(t, err)
	lnk, err := cid.Prefix{
		Version:  1,
		Codec:    0x71,
		MhType:   0x17,
		MhLength: 20,
	}.Sum(out.Bytes())
	require.NoError(t, err)
	blk, err := blocks.NewBlockWithCid(out.Bytes(), lnk)
	require.NoError(t, err)
	bsrv.AddBlock(blk)
	fetcherFactory := bsfetcher.NewFetcherConfig(bsrv)
	resolver := resolver.NewBasicResolver(fetcherFactory)

	rp1, remainder, err := resolver.ResolveToLastNode(ctx, path.FromString(lnk.String()+"/foo/bar"))
	require.NoError(t, err)

	assert.Equal(t, lnk, rp1)
	require.Equal(t, "foo/bar", path.Join(remainder))
}

func mockBlockService() blockservice.BlockService {
	bstore := blockstore.NewBlockstore(dssync.MutexWrap(ds.NewMapDatastore()))
	return blockservice.New(bstore, offline.Exchange(bstore))
}
