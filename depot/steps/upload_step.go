package steps

import (
	"archive/tar"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"

	"github.com/cloudfoundry-incubator/executor/depot/log_streamer"
	"github.com/cloudfoundry-incubator/executor/depot/uploader"
	garden_api "github.com/cloudfoundry-incubator/garden/api"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/archiver/compressor"
	"github.com/pivotal-golang/bytefmt"
	"github.com/pivotal-golang/lager"
)

type uploadStep struct {
	container   garden_api.Container
	model       models.UploadAction
	uploader    uploader.Uploader
	compressor  compressor.Compressor
	tempDir     string
	streamer    log_streamer.LogStreamer
	rateLimiter chan struct{}
	logger      lager.Logger
	cancel      chan struct{}
}

func NewUpload(
	container garden_api.Container,
	model models.UploadAction,
	uploader uploader.Uploader,
	compressor compressor.Compressor,
	tempDir string,
	streamer log_streamer.LogStreamer,
	rateLimiter chan struct{},
	logger lager.Logger,
) *uploadStep {
	logger = logger.Session("upload-step", lager.Data{
		"from": model.From,
	})

	return &uploadStep{
		container:   container,
		model:       model,
		uploader:    uploader,
		compressor:  compressor,
		tempDir:     tempDir,
		streamer:    streamer,
		rateLimiter: rateLimiter,
		logger:      logger,

		cancel: make(chan struct{}),
	}
}

const (
	ErrCreateTmpDir    = "Failed to create temp dir"
	ErrEstablishStream = "Failed to establish stream from container"
	ErrReadTar         = "Failed to find first item in tar stream"
	ErrCreateTmpFile   = "Failed to create temp file"
	ErrCopyStreamToTmp = "Failed to copy stream contents into temp file"
)

func (step *uploadStep) Perform() (err error) {
	step.rateLimiter <- struct{}{}
	defer func() {
		<-step.rateLimiter
	}()

	step.logger.Info("upload-starting")
	step.emit("Uploading %s...\n", step.model.Artifact)

	url, err := url.ParseRequestURI(step.model.To)
	if err != nil {
		step.logger.Info("failed-to-parse-url")
		// Do not emit error in case it leaks sensitive data in URL
		return err
	}

	tempDir, err := ioutil.TempDir(step.tempDir, "upload")
	if err != nil {
		return NewEmittableError(err, ErrCreateTmpDir)
	}

	defer os.RemoveAll(tempDir)

	outStream, err := step.container.StreamOut(step.model.From)
	if err != nil {
		step.logger.Info("failed-to-stream-out")
		return NewEmittableError(err, ErrEstablishStream)
	}
	defer outStream.Close()

	tarStream := tar.NewReader(outStream)
	_, err = tarStream.Next()
	if err != nil {
		step.logger.Info("failed-to-read-stream")
		return NewEmittableError(err, ErrReadTar)
	}

	tempFile, err := ioutil.TempFile(step.tempDir, "compressed")
	if err != nil {
		return NewEmittableError(err, ErrCreateTmpFile)
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, tarStream)
	if err != nil {
		return NewEmittableError(err, ErrCopyStreamToTmp)
	}
	finalFileLocation := tempFile.Name()

	defer os.RemoveAll(finalFileLocation)

	uploadedBytes, err := step.uploader.Upload(finalFileLocation, url, step.cancel)
	if err != nil {
		step.logger.Info("failed-to-upload")
		// Do not emit error in case it leaks sensitive data in URL
		step.emit("Failed to upload %s\n", step.model.Artifact)
		return err
	}

	step.emit("Uploaded %s (%s)\n", step.model.Artifact, bytefmt.ByteSize(uint64(uploadedBytes)))

	step.logger.Info("upload-successful")
	return nil
}

func (step *uploadStep) Cancel() {
	close(step.cancel)
}

func (step *uploadStep) emit(format string, a ...interface{}) {
	if step.model.Artifact != "" {
		fmt.Fprintf(step.streamer.Stdout(), format, a...)
	}
}