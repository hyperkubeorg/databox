{{/*
Expand the name of the chart.
*/}}
{{- define "pcp-runner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "pcp-runner.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "pcp-runner.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "pcp-runner.labels" -}}
helm.sh/chart: {{ include "pcp-runner.chart" . }}
{{ include "pcp-runner.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "pcp-runner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pcp-runner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "pcp-runner.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pcp-runner.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The Secret holding the pairing setup code: an operator-supplied existing
Secret, or the one this chart creates.
*/}}
{{- define "pcp-runner.setupSecretName" -}}
{{- if .Values.pairing.existingSecret }}
{{- .Values.pairing.existingSecret }}
{{- else }}
{{- printf "%s-pairing" (include "pcp-runner.fullname" .) }}
{{- end }}
{{- end }}
