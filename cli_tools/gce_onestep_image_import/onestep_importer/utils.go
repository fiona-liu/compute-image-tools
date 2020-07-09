//  Copyright 2020 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package importer

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/compute-image-tools/cli_tools/common/utils/flags"
	"github.com/GoogleCloudPlatform/compute-image-tools/daisy"
	"github.com/dustin/go-humanize"
)

// runCmd executes the named program with given arguments.
// Logs the running command.
func runCmd(name string, args []string) error {
	log.Printf("Running command: '%s %s'", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// handleTimeout signals caller by closing the timeout channel after the
// specified time has elapsed.
func handleTimeout(timeout time.Duration, timeoutChan chan struct{}) {
	time.Sleep(timeout)
	close(timeoutChan)
}

// cloudProviderImporter represents the importer for various cloud providers.
type cloudProviderImporter interface {
	run(args *OneStepImportArguments) error
}

// newImporterFormCloudProvider evaluates the cloud provider of the source image
// and creates a new instance of cloudProviderImporter
func newImporterForCloudProvider(args *OneStepImportArguments) (cloudProviderImporter, error) {
	if args.CloudProvider == "aws" {
		importer, err := newAWSImporter(args.Oauth, args.TimeoutChan, newAWSImportArguments(args))
		if err != nil {
			return nil, err
		}
		return importer, nil
	}
	return nil, daisy.Errf("image import from cloud provider %v is "+
		"currently not supported", args.CloudProvider)
}

// importFromCloudProvider imports image from the specified cloud provider
func importFromCloudProvider(args *OneStepImportArguments) error {
	cleanup := sync.WaitGroup{}
	// 1. Get importer
	importer, err := newImporterForCloudProvider(args)
	if err != nil {
		return err
	}

	// 2. Run importer
	errChan := make(chan error, 1)
	cleanup.Add(1)
	go func() {
		errChan <- importer.run(args)
		cleanup.Done()
	}()

	// 3. Run timeout
	go handleTimeout(args.Timeout, args.TimeoutChan)

	// 4. Ensure timeout is not exceeded before workflow is finished
	select {
	case err := <-errChan:
		return err
	case <-args.TimeoutChan:
		// wait to make sure importer has cancelled running tasks
		cleanup.Wait()
		return daisy.Errf("timeout exceeded")
	}
}

// runImageImport calls image import
func runImageImport(args *OneStepImportArguments) error {
	imageImportPath := filepath.Join(filepath.Dir(args.ExecutablePath), "gce_vm_image_import")
	err := runCmd(imageImportPath, []string{
		fmt.Sprintf("-image_name=%v", args.ImageName),
		fmt.Sprintf("-client_id=%v", args.ClientID),
		fmt.Sprintf("-os=%v", args.OS),
		fmt.Sprintf("-source_file=%v", args.SourceFile),
		fmt.Sprintf("-no_guest_environment=%v", args.NoGuestEnvironment),
		fmt.Sprintf("-family=%v", args.Family),
		fmt.Sprintf("-description=%v", args.Description),
		fmt.Sprintf("-network=%v", args.Network),
		fmt.Sprintf("-subnet=%v", args.Subnet),
		fmt.Sprintf("-zone=%v", args.Zone),
		fmt.Sprintf("-timeout=%v", args.Timeout),
		fmt.Sprintf("-project=%v", *args.ProjectPtr),
		fmt.Sprintf("-scratch_bucket_gcs_path=%v", args.ScratchBucketGcsPath),
		fmt.Sprintf("-oauth=%v", args.Oauth),
		fmt.Sprintf("-compute_endpoint_override=%v", args.ComputeEndpoint),
		fmt.Sprintf("-disable_gcs_logging=%v", args.GcsLogsDisabled),
		fmt.Sprintf("-disable_cloud_logging=%v", args.CloudLogsDisabled),
		fmt.Sprintf("-disable_stdout_logging=%v", args.StdoutLogsDisabled),
		fmt.Sprintf("-no_external_ip=%v", args.NoExternalIP),
		fmt.Sprintf("-labels=%v", flags.KeyValueString(args.Labels).String()),
		fmt.Sprintf("-storage_location=%v", args.StorageLocation)})
	if err != nil {
		return daisy.Errf("failed to import image: %v", err)
	}
	return nil
}

// uploader is responsible for receiving file chunks and upload it to a destination
type uploader struct {
	readerChan    chan io.ReadCloser
	writer        io.WriteCloser
	totalUploaded int64
	totalFileSize int64
	uploadErr     error
	sync.Mutex
	sync.WaitGroup

	uploadFileFn func()
	cleanupFn    func()
}

// uploadFile uploads file chunks to writer
func (uploader *uploader) uploadFile() {
	// Used to test.
	if uploader.uploadFileFn != nil {
		uploader.uploadFileFn()
		return
	}

	defer uploader.Done()
	for reader := range uploader.readerChan {
		defer reader.Close()
		n, err := io.Copy(uploader.writer, reader)
		uploader.Lock()
		if err != nil {
			uploader.uploadErr = err
		}
		uploader.Unlock()
		uploader.totalUploaded += n
		log.Printf("Total written size: %v of %v.", humanize.IBytes(uint64(uploader.totalUploaded)), humanize.IBytes(uint64(uploader.totalFileSize)))
	}
}

// cleanup cleans up all resources.
func (uploader *uploader) cleanup() {
	// Used to test.
	if uploader.cleanupFn != nil {
		uploader.cleanupFn()
		return
	}

	uploader.Lock()
	close(uploader.readerChan)
	for reader := range uploader.readerChan {
		reader.Close()
	}
	uploader.writer.Close()
	uploader.Done()
	uploader.Unlock()
}
