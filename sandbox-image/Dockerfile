# syntax=docker/dockerfile:1
# =============================================================================
# Sandbox images (OPENSANDBOX_IMAGE), pre-baked so a new sandbox does no package
# install at start. OpenSandbox injects execd at runtime; these images only
# provide the toolchain the agent's `execute_command` relies on. Two targets
# share their layers (office is base plus more):
#
#   base    What every sandbox needs (docs/sandbox-adapters.md, section 3): bash,
#           GNU coreutils, findutils, util-linux, python3 + pypdf, ripgrep (search_files),
#           git, tar/gzip (workspace checkpoints), curl, file, unzip, column, pkill.
#   office  base plus the document and data stack:
#             - Python: python-pptx, openpyxl, xlsxwriter, python-docx, pdfplumber,
#               reportlab, markitdown, pandas, numpy, matplotlib, seaborn, plotly,
#               Pillow, lxml, beautifulsoup4, requests
#             - Node 22 + global pptxgenjs, docx, sharp (resolved through NODE_PATH)
#             - LibreOffice (soffice headless convert / recalc), poppler (pdftoppm),
#               pandoc, qpdf, ffmpeg, common fonts (+ Calibri/Cambria metric clones)
#
# Build (from this directory):
#   docker build --target base   -t harness-sandbox:base   .
#   docker build --target office -t harness-sandbox:office .
#   (a plain `docker build .` builds the last target, office)
#
# Behind a TLS-intercepting proxy or with a private mirror, without editing this file:
#   - drop the extra CA certificates (*.crt) into ./certs/
#   - --build-arg PIP_INDEX_URL=… --build-arg NPM_REGISTRY=…
#   - --build-arg NODE_IMAGE=… --build-arg PYTHON_IMAGE=… for mirrored base images
# =============================================================================
ARG NODE_IMAGE=node:22-bookworm
ARG PYTHON_IMAGE=python:3.11-slim-bookworm
FROM ${NODE_IMAGE} AS node

# -----------------------------------------------------------------------------
# base
# -----------------------------------------------------------------------------
FROM ${PYTHON_IMAGE} AS base
ARG PIP_INDEX_URL=

USER root
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt

# Extra CA certificates (corporate proxy, private registry): any *.crt in ./certs/
COPY certs/ /usr/local/share/ca-certificates/extra/

# The shell tools the agent reaches for: ripgrep (search_files), git (workspace checkpoints),
# file (type / encoding checks), unzip, column (bsdextrautils), pkill (procps). bash, GNU
# coreutils, findutils, util-linux (setsid), tar and gzip come with the Debian base.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ripgrep git curl ca-certificates \
        file unzip bsdextrautils procps \
    && update-ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# pypdf is small and pure Python: read_file extracts PDF text in every sandbox.
RUN pip install --upgrade pip \
    && pip install ${PIP_INDEX_URL:+--index-url "$PIP_INDEX_URL"} pypdf

# Fail the build if a tool the protocol scripts need is missing or is not the GNU one.
RUN bash --version >/dev/null && python3 -c "import pypdf" \
    && realpath -m -z /a/../b | tr '\0' '\n' | grep -qx /b \
    && stat -c '%s %Y' /etc/hostname >/dev/null && echo x | base64 -w0 >/dev/null \
    && touch /tmp/.a && mv -f -T /tmp/.a /tmp/.b && rm /tmp/.b \
    && find /tmp -mmin -1 -name nothing -delete && setsid true && sleep 0.1 \
    && tail -c 1 /etc/hostname >/dev/null && head -c 1 /etc/hostname >/dev/null \
    && git --version && curl --version >/dev/null && rg --version >/dev/null && tar --version >/dev/null \
    && gzip --version >/dev/null && file --version >/dev/null && pkill --version >/dev/null \
    && unzip -v >/dev/null && echo a | column -t >/dev/null

WORKDIR /workspace

# -----------------------------------------------------------------------------
# office (the default build)
# -----------------------------------------------------------------------------
FROM base AS office
ARG PIP_INDEX_URL=
ARG NPM_REGISTRY=
ENV NODE_PATH=/usr/local/lib/node_modules \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt

# Document tooling: LibreOffice (headless), PDF tools, pandoc, ffmpeg, fonts.
# Carlito / Caladea are metric-compatible with Calibri / Cambria (the Office defaults): without
# them uploaded documents and python-pptx's default theme render with different line breaks.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libreoffice-impress libreoffice-writer libreoffice-calc \
        poppler-utils qpdf pandoc ffmpeg \
        fonts-dejavu fonts-liberation fonts-liberation2 fonts-noto-core fonts-noto-color-emoji \
        fonts-crosextra-carlito fonts-crosextra-caladea \
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
RUN pip install ${PIP_INDEX_URL:+--index-url "$PIP_INDEX_URL"} \
        python-pptx openpyxl xlsxwriter python-docx \
        pdfplumber reportlab \
        "markitdown[pptx,docx,xlsx,pdf]" \
        pandas numpy matplotlib seaborn plotly \
        Pillow lxml beautifulsoup4 requests

# Fail the build if a tool the agent relies on is missing.
RUN python3 -c "import pptx, openpyxl, xlsxwriter, docx, pandas, numpy, matplotlib, pypdf, pdfplumber, reportlab, markitdown, PIL, lxml, bs4, requests" \
    && node -e "require('pptxgenjs'); require('docx'); require('sharp')" \
    && soffice --version && pdftoppm -v && pandoc --version >/dev/null && qpdf --version >/dev/null \
    && ffmpeg -version >/dev/null \
    && fc-list : family | grep -q "Liberation Sans" && fc-list : family | grep -q Carlito \
    && fc-list : family | grep -q Caladea
