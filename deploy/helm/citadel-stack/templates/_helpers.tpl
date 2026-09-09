{{/*
Expand the name of the chart.
*/}}
{{- define "citadel-stack.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name with a component suffix.
Use as: include "citadel-stack.componentName" (dict "root" . "component" "wiremock")
*/}}
{{- define "citadel-stack.componentName" -}}
{{- printf "%s-%s" .root.Release.Name .component | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every object.
*/}}
{{- define "citadel-stack.labels" -}}
app.kubernetes.io/part-of: citadel
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
citadel.io/owner: {{ .Values.owner | default "unknown" | quote }}
{{- end }}

{{/*
Selector labels for a component.
Use as: include "citadel-stack.selectorLabels" (dict "root" . "component" "wiremock")
*/}}
{{- define "citadel-stack.selectorLabels" -}}
app.kubernetes.io/name: {{ include "citadel-stack.componentName" . }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{/*
Pod security context. The restricted Pod Security Admission profile needs
runAsNonRoot and the RuntimeDefault seccomp profile.
No image in this chart declares a numeric non-root USER, so every caller passes
the uid and the gid of the account that its image holds.
Use as: include "citadel-stack.podSecurityContext" .Values.wiremock.podSecurityContext
*/}}
{{- define "citadel-stack.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- with . }}
{{ toYaml . | trim }}
{{- end }}
{{- end }}

{{/*
Container security context. Requires readOnlyRootFilesystem and drops all capabilities.
*/}}
{{- define "citadel-stack.containerSecurityContext" -}}
readOnlyRootFilesystem: true
allowPrivilegeEscalation: false
capabilities:
  drop:
    - ALL
{{- end }}

{{/*
The capture exchange list, in the comma separated name:mode form that
EVENT_CAPTURE_EXCHANGES takes. SDD section 8.1 defaults the mode to own.
*/}}
{{- define "citadel-stack.captureExchanges" -}}
{{- $entries := list -}}
{{- range .Values.eventCapture.exchanges -}}
{{- $entries = append $entries (printf "%s:%s" .name (.declare | default "own")) -}}
{{- end -}}
{{- join "," $entries -}}
{{- end }}

{{/*
Image pull secrets from global values.
*/}}
{{- define "citadel-stack.imagePullSecrets" -}}
{{- with .Values.global.imagePullSecrets -}}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}
