module github.com/xhd2015/deepseek-harness-go/compat

go 1.25.10

replace (
	github.com/xhd2015/agent-pro => ../external/agent-pro
	github.com/xhd2015/deepseek-harness-go => ../
)

require (
	github.com/xhd2015/agent-pro v0.0.0-00010101000000-000000000000
	github.com/xhd2015/deepseek-harness-go v0.0.0-00010101000000-000000000000
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/klauspost/compress v1.18.4 // indirect
	github.com/xhd2015/dot-pkgs/go-pkgs v0.0.205 // indirect
	golang.org/x/image v0.28.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
