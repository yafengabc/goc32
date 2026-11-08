module goc32

go 1.25.0

require (
	goc/common v0.0.0
	goc/frontend v0.0.0
	gocl v0.0.0
	native32 v0.0.0
)

require goa v0.0.0 // indirect

replace goc/common => ./internal/common

replace goc/frontend => ./internal/frontend

replace gocl => ./internal/ir

replace goa => ./internal/goa

replace native32 => ./internal/native32
