# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
# 生成と確かめの入口（基本設計書 第14章 8、全体実装設計書 5.1）。PYTHON と GO は手元の道具の場所に替えられる。
PYTHON ?= python3
GO ?= go
GOFMT ?= gofmt
GEN_COMMON := server/internal/common/gen
GEN_BOUNDARY := server/internal/boundary/gen
# 境界の生成の道具（版を固定。第14章 5.2）
OAPI_CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0

.PHONY: gen gen-check

# 境界の正本（api/）から生成ファイルを作る。生成ファイルはコミットしない（.gitignore）
gen:
	$(MAKE) gen-into COMMON=$(GEN_COMMON) BOUNDARY=$(GEN_BOUNDARY)

# 生成の再現性：2 つの先に作って比べる（第14章 8）
gen-check:
	rm -rf .gen-check-a .gen-check-b
	$(MAKE) gen-into COMMON=.gen-check-a/common BOUNDARY=.gen-check-a/boundary
	$(MAKE) gen-into COMMON=.gen-check-b/common BOUNDARY=.gen-check-b/boundary
	diff -r .gen-check-a .gen-check-b
	rm -rf .gen-check-a .gen-check-b

# 1 つの置き場に生成する（gen と gen-check が使う）。
# 1. エラーコード表、レート制限、設定の定数。2. openapi を束ねる。3. oapi-codegen。4. タグごとの interface と spec の取り込み（genimpl）。
.PHONY: gen-into
gen-into:
	$(PYTHON) -m tools.gen_errors --out $(COMMON)
	$(GOFMT) -w $(COMMON)
	$(PYTHON) -m tools.bundle_openapi --out $(BOUNDARY)
	cd server && $(GO) run $(OAPI_CODEGEN) -config $(abspath $(BOUNDARY))/oapi-codegen.yaml \
		-o $(abspath $(BOUNDARY))/boundary.gen.go $(abspath $(BOUNDARY))/openapi.bundled.yaml
	cd server && $(GO) run ./cmd/genimpl -gen $(abspath $(BOUNDARY))/boundary.gen.go \
		-spec $(abspath $(BOUNDARY))/openapi.bundled.yaml -out $(abspath $(BOUNDARY))/implementations.gen.go
