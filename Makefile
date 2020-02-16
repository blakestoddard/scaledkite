PROJECT = buildkite-agent-spawner
FUNCTION = $(PROJECT)
REGION = us-east-1

.phony: clean

clean:
	rm -f main main.zip -

build:
	GOOS=linux GOARCH=amd64 go build -o main main.go
	zip main.zip main

update:
	aws lambda update-function-code \
		--function-name $(FUNCTION) \
		--zip-file fileb://main.zip \
		--publish
