{{/*
Expand the name of the chart.
*/}}
{{- define "databox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "databox.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "databox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "databox.labels" -}}
helm.sh/chart: {{ include "databox.chart" . }}
{{ include "databox.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "databox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "databox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the headless Service backing StatefulSet pod DNS
(<pod>.<this-service>.<namespace>.svc.<clusterDomain>).
*/}}
{{- define "databox.headlessServiceName" -}}
{{- printf "%s-headless" (include "databox.fullname" .) }}
{{- end }}

{{/*
Name of the generated auth Secret (root password + node PSK).
*/}}
{{- define "databox.secretName" -}}
{{- printf "%s-auth" (include "databox.fullname" .) }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "databox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "databox.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
