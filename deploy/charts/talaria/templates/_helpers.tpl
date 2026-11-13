{{- define "talaria.fullname" -}}
{{- if contains "talaria" .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-talaria" .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "talaria.labels" -}}
app.kubernetes.io/name: talaria
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* talaria.selector: pass (list $ "component") */}}
{{- define "talaria.selector" -}}
app.kubernetes.io/name: talaria
app.kubernetes.io/instance: {{ (index . 0).Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end -}}

{{- define "talaria.publicUrl" -}}
{{- required "publicUrl is required, such as https://talaria.example.com or http://localhost:8080" .Values.publicUrl | trimSuffix "/" -}}
{{- end -}}

{{- define "talaria.host" -}}
{{- (urlParse (include "talaria.publicUrl" .)).host | splitList ":" | first -}}
{{- end -}}

{{- define "talaria.pullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "talaria.modelBaseUrl" -}}
{{- if .Values.model.baseUrl -}}
{{- .Values.model.baseUrl -}}
{{- else if .Values.demo.enabled -}}
{{- printf "http://%s-demo-llm:8000/v1" (include "talaria.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "talaria.modelName" -}}
{{- if .Values.model.name -}}
{{- .Values.model.name -}}
{{- else if .Values.demo.enabled -}}
talaria-demo
{{- end -}}
{{- end -}}
