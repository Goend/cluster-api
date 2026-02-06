/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudinit

// Encoding defines the cloud-init supported encodings for write_files content.
type Encoding string

const (
	// EncodingBase64 indicates the file content is base64 encoded.
	EncodingBase64 Encoding = "base64"
	// EncodingGzip indicates the file content is gzip encoded.
	EncodingGzip Encoding = "gzip"
	// EncodingGzipBase64 indicates the file content is gzip compressed and then base64 encoded.
	EncodingGzipBase64 Encoding = "gzip+base64"
)

// File describes a file rendered through cloud-init write_files.
type File struct {
	Path        string
	Owner       string
	Permissions string
	Encoding    Encoding
	Append      bool
	Content     string
}

// User captures the subset of cloud-init user configuration we currently support.
type User struct {
	Name              string
	Passwd            string
	Gecos             string
	Groups            string
	HomeDir           string
	Inactive          *bool
	LockPassword      *bool
	Shell             string
	PrimaryGroup      string
	Sudo              string
	SSHAuthorizedKeys []string
}
