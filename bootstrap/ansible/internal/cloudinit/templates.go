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

const (
	filesTemplate = `{{ define "files" -}}
write_files:{{ range . }}
-   path: {{.Path}}
    {{ if ne .Encoding "" -}}
    encoding: "{{.Encoding}}"
    {{ end -}}
    {{ if ne .Owner "" -}}
    owner: {{.Owner}}
    {{ end -}}
    {{ if ne .Permissions "" -}}
    permissions: '{{.Permissions}}'
    {{ end -}}
    {{ if .Append -}}
    append: true
    {{ end -}}
    content: |
{{.Content | Indent 6}}
{{- end -}}
{{- end -}}
`
	usersTemplate = `{{ define "users" -}}
{{- if . }}
users:{{ range . }}
  - name: {{ .Name }}
    {{- if .Passwd }}
    passwd: {{ .Passwd }}
    {{- end -}}
    {{- if .Gecos }}
    gecos: {{ .Gecos }}
    {{- end -}}
    {{- if .Groups }}
    groups: {{ .Groups }}
    {{- end -}}
    {{- if .HomeDir }}
    homedir: {{ .HomeDir }}
    {{- end -}}
    {{- if .Inactive }}
    inactive: true
    {{- end -}}
    {{- if .LockPassword }}
    lock_passwd: {{ .LockPassword }}
    {{- end -}}
    {{- if .Shell }}
    shell: {{ .Shell }}
    {{- end -}}
    {{- if .PrimaryGroup }}
    primary_group: {{ .PrimaryGroup }}
    {{- end -}}
    {{- if .Sudo }}
    sudo: {{ .Sudo }}
    {{- end -}}
    {{- if .SSHAuthorizedKeys }}
    ssh_authorized_keys:{{ range .SSHAuthorizedKeys }}
      - {{ . }}
    {{- end -}}
    {{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
`
	commandsTemplate = `{{ define "commands" -}}
{{ range . }}
  - {{printf "%q" .}}
{{- end -}}
{{- end -}}
`
	baseTemplate = `{{.Header}}
{{- if .WriteFiles }}
{{ template "files" .WriteFiles }}
{{- end }}
{{- if .RunCommands }}
runcmd:
{{ template "commands" .RunCommands }}
{{- end }}
{{ template "users" .Users }}
`
)
