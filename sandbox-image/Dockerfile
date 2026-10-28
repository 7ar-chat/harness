# syntax=docker/dockerfile:1
# =============================================================================
# Sandbox image (OPENSANDBOX_IMAGE), pre-baked so a new sandbox does no package
# install at start. OpenSandbox injects execd at runtime; this image only
# provides the toolchain the agent's `execute_command` relies on:
#   - Python 3.11: python-pptx, openpyxl, xlsxwriter, python-docx, pypdf,
#     pdfplumber, reportlab, markitdown, pandas, numpy, matplotlib, seaborn,
#     plotly, Pillow, lxml, beautifulsoup4, requests
#   - Node 22 + global pptxgenjs, docx, sharp (resolved through NODE_PATH)
#   - LibreOffice (soffice headless convert / recalc), poppler (pdftoppm),
#     pandoc, qpdf, ffmpeg, ripgrep (search_files), common fonts (+ Calibri/Cambria
#     metric clones), git, curl, file, unzip, column
#
# Build (from this directory):
#   docker build -t harness-sandbox:local .
#
# Behind a TLS-intercepting proxy or with a private mirror, without editing this file:
#   - drop the extra CA certificates (*.crt) into ./certs/
#   - --build-arg PIP_INDEX_URL=… --build-arg NPM_REGISTRY=…
#   - --build-arg NODE_IMAGE=… --build-arg PYTHON_IMAGE=… for mirrored base images
# =============================================================================
ARG NODE_IMAGE=node:22-bookworm
ARG PYTHON_IMAGE=python:3.11-bookworm
FROM ${NODE_IMAGE} AS node

FROM ${PYTHON_IMAGE}
ARG PIP_INDEX_URL=
ARG NPM_REGISTRY=

USER root
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    NODE_PATH=/usr/local/lib/node_modules \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt

# Extra CA certificates (corporate proxy, private registry): any *.crt in ./certs/
COPY certs/ /usr/local/share/ca-certificates/extra/

# Document tooling: LibreOffice (headless), PDF tools, pandoc, ffmpeg, fonts, and the shell tools
# the agent reaches for: file (type / encoding checks), unzip, column (bsdextrautils).
# Carlito / Caladea are metric-compatible with Calibri / Cambria (the Office defaults): without
# them uploaded documents and python-pptx's default theme render with different line breaks.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libreoffice-impress libreoffice-writer libreoffice-calc \
        poppler-utils qpdf pandoc ffmpeg ripgrep \
        fonts-dejavu fonts-liberation fonts-liberation2 fonts-noto-core fonts-noto-color-emoji \
        fonts-crosextra-carlito fonts-crosextra-caladea \
        git curl ca-certificates \
        file unzip bsdextrautils \
    && update-ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node 22 (Debian's nodejs is too old for pptxgenjs). The official-style node
# binary is built against an old glibc, so it runs on this bookworm base.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

# Global Node packages for the docx / pptx scripts.
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
    && npm install -g --no-fund --no-audit pptxgenjs docx sharp \
    && npm cache clean --force \
    && rm -f /root/.npmrc

# Python office automation, document analysis and data libraries
RUN pip install --upgrade pip \
    && pip install ${PIP_INDEX_URL:+--index-url "$PIP_INDEX_URL"} \
        python-pptx openpyxl xlsxwriter python-docx \
        pypdf pdfplumber reportlab \
        "markitdown[pptx,docx,xlsx,pdf]" \
        pandas numpy matplotlib seaborn plotly \
        Pillow lxml beautifulsoup4 requests

# Fail the build if a tool the agent relies on is missing.
RUN python3 -c "import pptx, openpyxl, xlsxwriter, docx, pandas, numpy, matplotlib, pypdf, pdfplumber, reportlab, markitdown, PIL, lxml, bs4, requests" \
    && node -e "require('pptxgenjs'); require('docx'); require('sharp')" \
    && soffice --version && pdftoppm -v && pandoc --version >/dev/null && qpdf --version >/dev/null \
    && ffmpeg -version >/dev/null && git --version && curl --version >/dev/null && rg --version >/dev/null \
    && file --version >/dev/null && unzip -v >/dev/null && echo a | column -t >/dev/null \
    && fc-list : family | grep -q "Liberation Sans" && fc-list : family | grep -q Carlito \
    && fc-list : family | grep -q Caladea

WORKDIR /workspace
