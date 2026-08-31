{{/*
cpu-idle-operator.port extracts the port segment from a listen address
value such as ":8080" or "0.0.0.0:8080", for use as a container port. The
agent's --metrics-addr/--health-addr flags and the DaemonSet's matching
containerPort fields must agree, so both are derived from the same value
instead of being configured twice.
*/}}
{{- define "cpu-idle-operator.port" -}}
{{- $parts := splitList ":" . -}}
{{- last $parts -}}
{{- end -}}
