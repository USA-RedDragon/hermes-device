{{- define "talaria.fullname" -}}
{{- if contains "talaria" .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-talaria" .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "talaria.publicUrl" -}}
{{- required "publicUrl is required, such as https://talaria.example.com or http://localhost:8080" .Values.publicUrl | trimSuffix "/" -}}
{{- end -}}

{{- define "talaria.host" -}}
{{- (urlParse (include "talaria.publicUrl" .)).host | splitList ":" | first -}}
{{- end -}}

{{/* talaria.image: an image reference such as repo:tag@sha256:... as the common library's {repository, tag, digest} */}}
{{- define "talaria.image" -}}
{{- $ref := index . 0 -}}
{{- $pull := index . 1 -}}
{{- $digest := "" -}}
{{- if contains "@" $ref -}}
{{- $digest = splitList "@" $ref | last -}}
{{- $ref = splitList "@" $ref | first -}}
{{- end -}}
{{- $tag := "" -}}
{{- $last := splitList "/" $ref | last -}}
{{- if contains ":" $last -}}
{{- $tag = splitList ":" $last | last -}}
{{- $ref = trimSuffix (printf ":%s" $tag) $ref -}}
{{- end -}}
{{- dict "repository" $ref "tag" $tag "digest" $digest "pullPolicy" $pull | toYaml -}}
{{- end -}}

{{/* talaria.binaryMode: how acp-host gets the talaria binary, "image" or "copy" */}}
{{- define "talaria.binaryMode" -}}
{{- $mode := .Values.hermes.editorBridge.binary -}}
{{- if not (has $mode (list "auto" "image" "copy")) -}}
{{- fail "hermes.editorBridge.binary must be auto, image or copy" -}}
{{- end -}}
{{- if eq $mode "auto" -}}
{{- /* Image volumes work without a feature gate from Kubernetes 1.35; older clusters get an init container that copies the binary */ -}}
{{- $mode = ternary "image" "copy" (semverCompare ">=1.35-0" .Capabilities.KubeVersion.Version) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}
