{{- /*
talaria.secrets: the common library's secrets, keys and config.

Every secret Talaria makes for itself lives in <fullname>-keys. It is made on the first install and read back on
every upgrade (lookup), so keys never change; uninstall keeps it (resource-policy keep). The config Secret is
rendered from it and from the values on every upgrade.
*/ -}}
{{- define "talaria.secrets" -}}
{{- $full := include "talaria.fullname" . -}}
{{- $name := printf "%s-keys" $full -}}
{{- $old := dict -}}
{{- with lookup "v1" "Secret" .Release.Namespace $name }}{{ $old = .data | default dict }}{{ end -}}
{{- $k := dict -}}
{{- $gen := dict
  "hub-signing-key" (genPrivateKey "ed25519")
  "vapid-key" (genPrivateKey "ecdsa")
  "session-secret" (randAlphaNum 48)
  "hermes-password" (randAlphaNum 32)
  "hermes-session-secret" (randAlphaNum 48)
  "sandbox-token" (printf "hdt_%s" (randAlphaNum 44))
  "searxng-secret" (randAlphaNum 48)
-}}
{{- range $key, $v := $gen }}
{{- if hasKey $old $key }}{{ $_ := set $k $key (index $old $key | b64dec) }}{{ else }}{{ $_ := set $k $key $v }}{{ end }}
{{- end }}
{{- $mode := .Values.auth.mode }}
{{- if not (has $mode (list "local" "proxy" "oidc")) }}
{{- fail "auth.mode must be local, proxy or oidc" }}
{{- end }}
keys:
  forceRename: {{ $name }}
  annotations:
    helm.sh/resource-policy: keep
  type: Opaque
  stringData: {{- toYaml $k | nindent 4 }}
config:
  forceRename: {{ $full }}-config
  type: Opaque
  stringData:
    hub.yaml: |
      auth:
        mode: {{ $mode }}
        # The front container sits in the same pod: read the client address from X-Forwarded-For
        trusted_proxies: {{ toJson .Values.auth.trustedProxies }}
        {{- with .Values.auth.admins }}
        admins: {{ toJson . }}
        {{- end }}
        {{- if eq $mode "proxy" }}
        proxy: {{- toYaml .Values.auth.proxy | nindent 10 }}
        {{- end }}
      {{- if eq $mode "oidc" }}
      oidc:
        issuer_url: {{ required "auth.oidc.issuerUrl is required with auth.mode oidc" .Values.auth.oidc.issuerUrl }}
        client_id: {{ required "auth.oidc.clientId is required with auth.mode oidc" .Values.auth.oidc.clientId }}
        {{- if not .Values.auth.oidc.existingSecret }}
        client_secret: {{ .Values.auth.oidc.clientSecret | quote }}
        {{- end }}
        {{- with .Values.auth.oidc.settings }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- end }}
      {{- with .Values.auth.users }}
      users: {{- toYaml . | nindent 8 }}
      {{- end }}
      session:
        secret: {{ get $k "session-secret" | quote }}
      device:
        signing_key: |
          {{- get $k "hub-signing-key" | nindent 10 }}
      notify:
        vapid_private_key: |
          {{- get $k "vapid-key" | nindent 10 }}
        {{- with .Values.pushContact }}
        vapid_subject: {{ . | quote }}
        {{- end }}
      {{- if and .Values.telegram.token (not .Values.telegram.existingSecret) }}
      telegram:
        enabled: true
        token: {{ .Values.telegram.token | quote }}
      {{- end }}
      backends:
        - id: main
          name: Talaria
          url: http://{{ $full }}-hermes:9120
          username: hub
          password: {{ get $k "hermes-password" | quote }}
          devices: true
          {{- if .Values.hermes.editorBridge.enabled }}
          acp_url: http://{{ $full }}-hermes:9130
          {{- end }}
          media_roots: [/opt/data/cache]
          service_devices:
            - id: sandbox
              name: Sandbox
              token: {{ get $k "sandbox-token" | quote }}
    hub-signing-key: |
      {{- get $k "hub-signing-key" | nindent 6 }}
    password: {{ get $k "hermes-password" | quote }}
    session-secret: {{ get $k "hermes-session-secret" | quote }}
    sandbox-token: {{ get $k "sandbox-token" | quote }}
    searxng-secret: {{ get $k "searxng-secret" | quote }}
    hermes-extra.yaml: |
      {{- toYaml .Values.hermes.config | nindent 6 }}
{{- end -}}
