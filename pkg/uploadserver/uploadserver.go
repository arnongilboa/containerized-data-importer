/*
 * This file is part of the CDI project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package uploadserver

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"k8s.io/klog"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/importer"
	"kubevirt.io/containerized-data-importer/pkg/util"
)

const (
	// UploadContentTypeHeader is the header upload clients may use to set the content type explicitly
	UploadContentTypeHeader = "x-cdi-content-type"

	// FilesystemCloneContentType is the content type when cloning a filesystem
	FilesystemCloneContentType = "filesystem-clone"

	// UploadPathSync is the path to POST CDI uploads
	UploadPathSync = "/v1beta1/upload"

	// UploadPathAsync is the path to POST CDI uploads in async mode
	UploadPathAsync = "/v1beta1/upload-async"

	// UploadFormSync is the path to POST CDI uploads as form data
	UploadFormSync = "/v1beta1/upload-form"

	// UploadFormAsync is the path to POST CDI uploads as form datain async mode
	UploadFormAsync = "/v1beta1/upload-form-async"

	healthzPort = 8080
	healthzPath = "/healthz"
)

// ProxyPaths are all supported paths
var ProxyPaths = append(
	append(syncUploadPaths, asyncUploadPaths...),
	append(syncUploadFormPaths, asyncUploadFormPaths...)...,
)

var syncUploadPaths = []string{
	UploadPathSync,
	"/v1alpha1/upload",
}

var asyncUploadPaths = []string{
	UploadPathAsync,
	"/v1alpha1/upload-async",
}

var syncUploadFormPaths = []string{
	UploadFormSync,
	"/v1alpha1/upload-form",
}

var asyncUploadFormPaths = []string{
	UploadFormAsync,
	"/v1alpha1/upload-form-async",
}

// UploadServer is the interface to uploadServerApp
type UploadServer interface {
	Run() error
}

type uploadServerApp struct {
	bindAddress string
	bindPort    int
	destination string
	tlsKey      string
	tlsCert     string
	clientCert  string
	clientName  string
	keyFile     string
	certFile    string
	imageSize   string
	mux         *http.ServeMux
	uploading   bool
	processing  bool
	done        bool
	doneChan    chan struct{}
	errChan     chan error
	mutex       sync.Mutex
}

type imageReadCloser func(*http.Request) (io.ReadCloser, error)

// may be overridden in tests
var uploadProcessorFunc = newUploadStreamProcessor
var uploadProcessorFuncAsync = newAsyncUploadStreamProcessor

func bodyReadCloser(r *http.Request) (io.ReadCloser, error) {
	return r.Body, nil
}

func formReadCloser(r *http.Request) (io.ReadCloser, error) {
	multiReader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}

	var filePart *multipart.Part

	for {
		filePart, err = multiReader.NextPart()
		if err != nil || filePart.FormName() == "file" {
			break
		}
		klog.Infof("Ignoring part %s", filePart.FormName())
	}

	// multiReader.NextPart() returns io.EOF when read everything
	if err != nil {
		return nil, err
	}

	return filePart, nil
}

// NewUploadServer returns a new instance of uploadServerApp
func NewUploadServer(bindAddress string, bindPort int, destination, tlsKey, tlsCert, clientCert, clientName, imageSize string) UploadServer {
	server := &uploadServerApp{
		bindAddress: bindAddress,
		bindPort:    bindPort,
		destination: destination,
		tlsKey:      tlsKey,
		tlsCert:     tlsCert,
		clientCert:  clientCert,
		clientName:  clientName,
		imageSize:   imageSize,
		mux:         http.NewServeMux(),
		uploading:   false,
		done:        false,
		doneChan:    make(chan struct{}),
		errChan:     make(chan error),
	}

	for _, path := range syncUploadPaths {
		server.mux.HandleFunc(path, server.uploadHandler(bodyReadCloser))
	}
	for _, path := range asyncUploadPaths {
		server.mux.HandleFunc(path, server.uploadHandlerAsync(bodyReadCloser))
	}
	for _, path := range syncUploadFormPaths {
		server.mux.HandleFunc(path, server.uploadHandler(formReadCloser))
	}
	for _, path := range asyncUploadFormPaths {
		server.mux.HandleFunc(path, server.uploadHandlerAsync(formReadCloser))
	}

	return server
}

func (app *uploadServerApp) Run() error {
	uploadServer, err := app.createUploadServer()
	if err != nil {
		return errors.Wrap(err, "Error creating upload http server")
	}

	healthzServer, err := app.createHealthzServer()
	if err != nil {
		return errors.Wrap(err, "Error creating healthz http server")
	}

	uploadListener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", app.bindAddress, app.bindPort))
	if err != nil {
		return errors.Wrap(err, "Error creating upload listerner")
	}

	healthzListener, err := net.Listen("tcp", fmt.Sprintf(":%d", healthzPort))
	if err != nil {
		return errors.Wrap(err, "Error creating healthz listerner")
	}

	go func() {
		defer uploadListener.Close()

		// maybe bind port was 0 (unit tests) assign port here
		app.bindPort = uploadListener.Addr().(*net.TCPAddr).Port

		if app.keyFile != "" && app.certFile != "" {
			app.errChan <- uploadServer.ServeTLS(uploadListener, app.certFile, app.keyFile)
			return
		}

		// not sure we want to support this code path
		app.errChan <- uploadServer.Serve(uploadListener)
	}()

	go func() {
		defer healthzServer.Close()

		app.errChan <- healthzServer.Serve(healthzListener)
	}()

	select {
	case err = <-app.errChan:
		klog.Errorf("HTTP server returned error %s", err.Error())
	case <-app.doneChan:
		klog.Info("Shutting down http server after successful upload")
		healthzServer.Shutdown(context.Background())
		uploadServer.Shutdown(context.Background())
	}

	return err
}

func (app *uploadServerApp) createUploadServer() (*http.Server, error) {
	server := &http.Server{
		Handler: app,
	}

	if app.tlsKey != "" && app.tlsCert != "" {
		certDir, err := ioutil.TempDir("", "uploadserver-tls")
		if err != nil {
			return nil, errors.Wrap(err, "Error creating cert dir")
		}

		app.keyFile = filepath.Join(certDir, "tls.key")
		app.certFile = filepath.Join(certDir, "tls.crt")

		err = ioutil.WriteFile(app.keyFile, []byte(app.tlsKey), 0600)
		if err != nil {
			return nil, errors.Wrap(err, "Error creating key file")
		}

		err = ioutil.WriteFile(app.certFile, []byte(app.tlsCert), 0600)
		if err != nil {
			return nil, errors.Wrap(err, "Error creating cert file")
		}
	}

	if app.clientCert != "" {
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM([]byte(app.clientCert)); !ok {
			klog.Fatalf("Invalid ca cert file %s", app.clientCert)
		}

		server.TLSConfig = &tls.Config{
			ClientCAs:  caCertPool,
			ClientAuth: tls.RequireAndVerifyClientCert,
		}
	}

	return server, nil
}

func (app *uploadServerApp) createHealthzServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, app.healthzHandler)
	return &http.Server{Handler: mux}, nil
}

func (app *uploadServerApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.mux.ServeHTTP(w, r)
}

func (app *uploadServerApp) healthzHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "OK")
}

func (app *uploadServerApp) validateShouldHandleRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusNotFound)
		return false
	}

	if r.TLS != nil {
		found := false

		for _, cert := range r.TLS.PeerCertificates {
			if cert.Subject.CommonName == app.clientName {
				found = true
				break
			}
		}

		if !found {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
	} else {
		klog.V(3).Infof("Handling HTTP connection")
	}

	app.mutex.Lock()
	defer app.mutex.Unlock()

	if app.uploading || app.processing {
		klog.Warning("Got concurrent upload request")
		w.WriteHeader(http.StatusServiceUnavailable)
		return false
	}

	if app.done {
		klog.Warning("Got upload request after already done")
		w.WriteHeader(http.StatusConflict)
		return false
	}

	app.uploading = true

	return true
}

func (app *uploadServerApp) uploadHandlerAsync(irc imageReadCloser) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if !app.validateShouldHandleRequest(w, r) {
			return
		}

		cdiContentType := r.Header.Get(UploadContentTypeHeader)

		klog.Infof("Content type header is %q\n", cdiContentType)

		readCloser, err := irc(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		}

		processor, err := uploadProcessorFuncAsync(readCloser, app.destination, app.imageSize, cdiContentType)

		app.mutex.Lock()

		if err != nil {
			klog.Errorf("Saving stream failed: %s", err)
			if _, ok := err.(importer.ValidationSizeError); ok {
				w.WriteHeader(http.StatusBadRequest)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
			w.Write([]byte(fmt.Sprintf("Saving stream failed: %s", err.Error())))
			app.uploading = false
			app.mutex.Unlock()
			return
		}
		defer app.mutex.Unlock()

		app.uploading = false
		app.processing = true

		// Start processing.
		go func() {
			defer close(app.doneChan)
			if err := processor.ProcessDataResume(); err != nil {
				klog.Errorf("Error during resumed processing: %v", err)
				app.errChan <- err
			}
			app.mutex.Lock()
			defer app.mutex.Unlock()
			app.processing = false
			app.done = true
			klog.Infof("Wrote data to %s", app.destination)
		}()

		klog.Info("Returning success to caller, continue processing in background")
	}
}

func (app *uploadServerApp) uploadHandler(irc imageReadCloser) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !app.validateShouldHandleRequest(w, r) {
			return
		}

		cdiContentType := r.Header.Get(UploadContentTypeHeader)

		klog.Infof("Content type header is %q\n", cdiContentType)

		readCloser, err := irc(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		}

		err = uploadProcessorFunc(readCloser, app.destination, app.imageSize, cdiContentType)

		app.mutex.Lock()
		defer app.mutex.Unlock()

		if err != nil {
			klog.Errorf("Saving stream failed: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			app.uploading = false
			return
		}

		app.uploading = false
		app.done = true

		close(app.doneChan)

		klog.Infof("Wrote data to %s", app.destination)
	}
}

func newAsyncUploadStreamProcessor(stream io.ReadCloser, dest, imageSize, contentType string) (*importer.DataProcessor, error) {
	if contentType == FilesystemCloneContentType {
		return nil, fmt.Errorf("async filesystem clone not supported")
	}

	uds := importer.NewAsyncUploadDataSource(stream)
	processor := importer.NewDataProcessor(uds, dest, common.ImporterVolumePath, common.ScratchDataDir, imageSize)
	return processor, processor.ProcessDataWithPause()
}

func newUploadStreamProcessor(stream io.ReadCloser, dest, imageSize, contentType string) error {
	if contentType == FilesystemCloneContentType {
		return filesystemCloneProcessor(stream, common.ImporterVolumePath)
	}

	uds := importer.NewUploadDataSource(stream)
	processor := importer.NewDataProcessor(uds, dest, common.ImporterVolumePath, common.ScratchDataDir, imageSize)
	return processor.ProcessData()
}

func filesystemCloneProcessor(stream io.ReadCloser, destDir string) error {
	if err := importer.CleanDir(destDir); err != nil {
		return errors.Wrapf(err, "error removing contents of %s", destDir)
	}

	gzr, err := gzip.NewReader(stream)
	if err != nil {
		return errors.Wrap(err, "error creating gzip reader")
	}

	if err = util.UnArchiveTar(gzr, destDir); err != nil {
		return errors.Wrapf(err, "error unarchiving to %s", destDir)
	}

	return nil
}
