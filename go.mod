module nre-ai-proxy

go 1.23.3

toolchain go1.24.0

replace github.com/ZergsLaw/dify-sdk-go v0.0.0-20250211202422-81ff88243182 => ./override/dify-sdk-go

require (
	github.com/ZergsLaw/dify-sdk-go v0.0.0-20250211202422-81ff88243182 // indirect
	github.com/cohesion-org/deepseek-go v1.2.4 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
)
