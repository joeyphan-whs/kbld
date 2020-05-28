package tarball

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	regv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/k14s/kbld/pkg/kbld/util"
)

type Logger interface {
	WriteStr(str string, args ...interface{}) error
}

type TarWriterOpts struct {
	Concurrency int
}

type TarWriter struct {
	ids       *ImageRefDescriptors
	dstOpener func() (io.WriteCloser, error)

	dst           io.WriteCloser
	tf            *tar.Writer
	layersToWrite []ImageLayerTarDescriptor

	opts   TarWriterOpts
	logger Logger
}

func NewTarWriter(ids *ImageRefDescriptors, dstOpener func() (io.WriteCloser, error),
	opts TarWriterOpts, logger Logger) *TarWriter {
	return &TarWriter{ids: ids, dstOpener: dstOpener, opts: opts, logger: logger}
}

func (w *TarWriter) Write() error {
	var err error

	w.dst, err = w.dstOpener()
	if err != nil {
		return err
	}

	defer w.dst.Close()

	w.tf = tar.NewWriter(w.dst)
	defer w.tf.Close()

	idsBytes, err := w.ids.AsBytes()
	if err != nil {
		return err
	}

	err = w.writeTarEntry(w.tf, "manifest.json", bytes.NewReader(idsBytes), int64(len(idsBytes)))
	if err != nil {
		return err
	}

	for _, td := range w.ids.descs {
		switch {
		case td.Image != nil:
			err := w.writeImage(*td.Image)
			if err != nil {
				return err
			}

		case td.ImageIndex != nil:
			err := w.writeImageIndex(*td.ImageIndex)
			if err != nil {
				return err
			}

		default:
			panic("Unknown item")
		}
	}

	return w.writeLayers()
}

func (w *TarWriter) writeImageIndex(td ImageIndexTarDescriptor) error {
	for _, idx := range td.Indexes {
		err := w.writeImageIndex(idx)
		if err != nil {
			return err
		}
	}

	for _, img := range td.Images {
		err := w.writeImage(img)
		if err != nil {
			return err
		}
	}

	return nil
}

func (w *TarWriter) writeImage(td ImageTarDescriptor) error {
	for _, imgLayer := range td.Layers {
		// TODO anything else we can do to deal with this?
		// Do not include foreign layers since we cannot
		// import without changing image digest.
		if imgLayer.IsDistributable() {
			w.layersToWrite = append(w.layersToWrite, imgLayer)
		}
	}
	return nil
}

type writtenLayer struct {
	Name   string
	Offset int64
	Layer  ImageLayerTarDescriptor
}

func (w *TarWriter) writeLayers() error {
	// Sort layers by digest to have deterministic archive
	sort.Slice(w.layersToWrite, func(i, j int) bool {
		return w.layersToWrite[i].Digest < w.layersToWrite[j].Digest
	})

	seekableDst, isSeekable := w.dst.(*os.File)
	isInflattable := (w.opts.Concurrency > 1) && isSeekable
	writtenLayers := map[string]writtenLayer{}

	// Inflate tar file so that multiple writes can happen in parallel
	for _, imgLayer := range w.layersToWrite {
		digest, err := regv1.NewHash(imgLayer.Digest)
		if err != nil {
			return err
		}

		name := digest.Algorithm + "-" + digest.Hex + ".tar.gz"

		// Dedup layers
		if _, found := writtenLayers[name]; found {
			continue
		}

		err = w.tf.Flush()
		if err != nil {
			return err
		}

		var stream io.Reader
		var currPos int64

		if isSeekable {
			currPos, err = seekableDst.Seek(0, 1)
			if err != nil {
				return fmt.Errorf("Find current pos: %s", err)
			}
		}

		if isInflattable {
			stream = io.LimitReader(zeroReader{}, imgLayer.Size)
		} else {
			foundLayer, err := w.ids.FindLayer(imgLayer)
			if err != nil {
				return err
			}

			stream, err = foundLayer.Open()
			if err != nil {
				return err
			}
		}

		err = w.writeTarEntry(w.tf, name, stream, imgLayer.Size)
		if err != nil {
			return fmt.Errorf("Writing tar entry: %s", err)
		}

		writtenLayers[name] = writtenLayer{
			Name:   name,
			Layer:  imgLayer,
			Offset: currPos,
		}
	}

	err := w.tf.Flush()
	if err != nil {
		return err
	}

	if isInflattable {
		return w.fillInLayers(writtenLayers)
	}

	return nil
}

func (w *TarWriter) fillInLayers(writtenLayers map[string]writtenLayer) error {
	var sortedWritterLayers []writtenLayer

	for _, writtenLayer := range writtenLayers {
		sortedWritterLayers = append(sortedWritterLayers, writtenLayer)
	}

	// Prefer larger sizes first
	sort.Slice(sortedWritterLayers, func(i, j int) bool {
		return sortedWritterLayers[i].Layer.Size >= sortedWritterLayers[j].Layer.Size
	})

	errCh := make(chan error, len(writtenLayers))
	writeThrottle := util.NewThrottle(w.opts.Concurrency)

	// Fill in actual data
	for _, writtenLayer := range sortedWritterLayers {
		writtenLayer := writtenLayer // copy

		go func() {
			writeThrottle.Take()
			defer writeThrottle.Done()

			errCh <- w.fillInLayer(writtenLayer)
		}()
	}

	for i := 0; i < len(writtenLayers); i++ {
		err := <-errCh
		if err != nil {
			return fmt.Errorf("Filling in a layer: %s", err)
		}
	}

	return nil
}

func (w *TarWriter) fillInLayer(wl writtenLayer) error {
	file, err := w.dstOpener()
	if err != nil {
		return err
	}

	defer file.Close()

	_, err = file.(*os.File).Seek(wl.Offset, 0)
	if err != nil {
		return fmt.Errorf("Seeking to offset: %s", err)
	}

	tw := tar.NewWriter(file)
	// Do not close tar writer as it would add unwanted footer

	foundLayer, err := w.ids.FindLayer(wl.Layer)
	if err != nil {
		return err
	}

	stream, err := foundLayer.Open()
	if err != nil {
		return err
	}

	err = w.writeTarEntry(tw, wl.Name, stream, wl.Layer.Size)
	if err != nil {
		return fmt.Errorf("Rewriting tar entry (%s): %s", wl.Name, err)
	}

	return tw.Flush()
}

func (w *TarWriter) writeTarEntry(tw *tar.Writer, path string, r io.Reader, size int64) error {
	hdr := &tar.Header{
		Mode:     0644,
		Typeflag: tar.TypeReg,
		Size:     size,
		Name:     path,
	}

	err := tw.WriteHeader(hdr)
	if err != nil {
		return fmt.Errorf("Writing header: %s", err)
	}

	t1 := time.Now()

	_, err = io.Copy(tw, r)
	if err != nil {
		return fmt.Errorf("Copying data: %s", err)
	}

	w.logger.WriteStr("done: file '%s' (%s)\n", path, time.Now().Sub(t1))

	return nil
}

type zeroReader struct{}

func (r zeroReader) Read(p []byte) (n int, err error) {
	for i, _ := range p {
		p[i] = 0
	}
	return len(p), nil
}