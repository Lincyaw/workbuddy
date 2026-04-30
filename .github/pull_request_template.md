## Summary

<!-- One paragraph: what changed and why. Reference the issue (`Fixes #N`). -->

## Test plan

- [ ] `make build`
- [ ] `go test ./... -count=1`
- [ ] `go vet ./...`
- [ ] `golangci-lint run ./...`
- [ ] `go run . validate-docs --strict`
- [ ] `python3 scripts/validate_index.py project-index.yaml`

## Compatibility

- [ ] schema/API 兼容（只加列、加端点，不删/不改字段）
