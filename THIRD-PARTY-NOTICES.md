# 第三方组件声明（Third-Party Notices）

本文件与 LICENSE 同级维护：LICENSE 约束本产品自身代码（Apache-2.0），
本文件覆盖随产品分发的第三方组件。分发渠道对应：桌面安装包 = §1–§6；安卓 APK = 另加 §7。

本产品包含以下第三方开源组件。除特别标注外均为原样分发、未作修改；
各组件著作权归其版权人所有，本声明不改变其许可条款。

## 1. Go 依赖（编译进主程序）

| 组件 | 版权 | License | 来源 |
|---|---|---|---|
| chromedp / cdproto / sysutil | Copyright (c) 2016-2025 Kenneth Shaw | MIT | https://github.com/chromedp/chromedp |
| goja | Copyright (c) 2016 Dmitry Panov | MIT | https://github.com/dop251/goja |
| gobwas/ws（含 httphead、pool） | Copyright (c) 2017-2021 Sergey Kamardin | MIT | https://github.com/gobwas/ws |
| go-webview2（本地补丁版） | Copyright (c) 2020 John Chadwick; 部分来自 (c) 2017 Serge Zaitsev | MIT | https://github.com/jchv/go-webview2 （本地改动见仓库 第三方库/go-webview2） |
| go-winloader | Copyright (c) 2021 John Chadwick | ISC | https://github.com/jchv/go-winloader |
| Pion WebRTC 全家（webrtc/ice/dtls/sctp/srtp/rtp/rtcp/sdp/stun/turn/transport/interceptor/datachannel/mdns/logging/randutil） | Copyright (c) The Pion community | MIT | https://github.com/pion |
| pkg/sftp | Copyright (c) 2013 Dave Cheney | BSD-3-Clause | https://github.com/pkg/sftp |
| androidbinary | Copyright (c) 2014 Ichinose Shogo | MIT | https://github.com/shogo82148/androidbinary |
| smallstep/pkcs7 | Copyright (c) 2015 Andrew Smith | MIT | https://github.com/smallstep/pkcs7 |
| wlynxg/anet | Copyright (c) 2023 wlynxg | BSD-3-Clause | https://github.com/wlynxg/anet |
| golang.org/x/crypto, /sys, /text, /net, /time | Copyright The Go Authors | BSD-3-Clause | https://go.dev |
| modernc.org/sqlite（含 libc/memory/mathutil） | Copyright (c) The Sqlite/Libc/Memory Authors | BSD-3-Clause | https://gitlab.com/cznic/sqlite |
| dlclark/regexp2 | Copyright (c) Doug Clark | MIT | https://github.com/dlclark/regexp2 |
| dustin/go-humanize | Copyright (c) 2005-2008 Dustin Sallings | MIT | https://github.com/dustin/go-humanize |
| go-json-experiment/json | Copyright (c) 2020 The Go Authors | BSD-3-Clause | https://github.com/go-json-experiment/json |
| go-sourcemap/sourcemap | Copyright (c) 2016 Contributors | BSD-3-Clause | https://github.com/go-sourcemap/sourcemap |
| google/pprof | Copyright (c) Google | Apache-2.0 | https://github.com/google/pprof |
| google/uuid | Copyright (c) 2009,2014 Google Inc. | BSD-3-Clause | https://github.com/google/uuid |
| kr/fs | Copyright (c) 2012 The Go Authors | BSD-3-Clause | https://github.com/kr/fs |
| mattn/go-isatty | Copyright (c) Yasuhiro Matsumoto | MIT | https://github.com/mattn/go-isatty |
| ncruces/go-strftime | Copyright (c) 2022 Nuno Cruces | MIT | https://github.com/ncruces/go-strftime |
| remyoudompheng/bigfft | Copyright (c) 2012 The Go Authors | BSD-3-Clause | https://github.com/remyoudompheng/bigfft |

## 2. 便携 Python 运行时（技能/_runtime）

| 组件 | 版本 | 版权 | License | 来源 |
|---|---|---|---|---|
| CPython | 3.14 | Copyright (c) Python Software Foundation | PSF License（全文见 技能/_runtime/python/LICENSE.txt，含 CNRI/BeOpen 历史条款） | https://www.python.org |
| OpenSSL（libcrypto-3.dll / libssl-3.dll） | 3.x | Copyright (c) 1998-2025 The OpenSSL Project | Apache-2.0 | https://www.openssl.org |
| libffi | 8 | Copyright (c) 1996-2025 Anthony Green / Red Hat | MIT | https://github.com/libffi/libffi |
| libtommath | — | Public domain (Unlicense) | Unlicense | https://github.com/libtom/libtommath |
| SQLite（sqlite3.dll） | — | Public domain | — | https://www.sqlite.org |
| Microsoft VC++ Runtime（vcruntime140*.dll） | — | Copyright (c) Microsoft Corporation | 微软可再分发条款 | https://www.microsoft.com |
| get-pip.py | — | Copyright (c) Python Packaging Authority | MIT（嵌入部分 PSF） | https://bootstrap.pypa.io |

### 2.1 site-packages（共享运行时，45 包）

| 组件 | 版本 | License | 版权 | 来源 |
|---|---|---|---|---|
| bcrypt | 5.0.0 | Apache-2.0 | Copyright (c) 2013-2025 Solar Designer; py bcrypt (c) 2013-2025 Donald Stufft | https://pypi.org/project/bcrypt/ |
| certifi | 2026.10.23 | MPL-2.0 **[MPL]** | Copyright (c) Certifi contributors | https://github.com/certifi/python-certifi |
| cffi | 2.1.1 | MIT-0 | Copyright (c) 2012-2025 Armin Ronacher & CFFI contributors | https://pypi.org/project/cffi/ |
| charset-normalizer | 3.5.2 | MIT |  | https://pypi.org/project/charset-normalizer/ |
| colorama | 0.4.6 | (见上游) | Copyright (c) 2010 Jonathan Hartley | https://github.com/tartley/colorama |
| comtypes | 1.4.17 | MIT | Copyright (c) 2006-2025 Enthought, Inc. | https://github.com/enthought/comtypes |
| cryptography | 50.0.2 | Apache-2.0 OR BSD-3-Clause | Copyright (c) 2013-2025 Individual contributors | https://pypi.org/project/cryptography/ |
| et_xmlfile | 2.0.0 | MIT | Copyright (c) 2011 openpyxl | https://pypi.org/project/et_xmlfile/ |
| flatbuffers | 25.12.19 | Apache 2.0 | Copyright (c) 2014-2025 Google Inc. (Apache-2.0) | https://pypi.org/project/flatbuffers/ |
| idna | 3.20 | BSD-3-Clause | Copyright (c) 2013-2025 Kim Davies | https://github.com/kjd/idna |
| lxml | 6.1.3 | BSD-3-Clause | Copyright (c) 2004 Infrae; ElementTree (c) 1999-2004 Fredrik Lundh | https://pypi.org/project/lxml/ |
| numpy | 2.5.4 | BSD-3-Clause AND 0BSD AND MIT AND Zlib AND CC0-1.0 | Copyright (c) 2005-2025, NumPy Developers | https://pypi.org/project/numpy/ |
| onnxruntime | 1.31.0 | MIT License | Copyright (c) Microsoft Corporation | https://pypi.org/project/onnxruntime/ |
| opencv-python | 5.0.0.93 | Apache 2.0 | : https://github.com/opencv/opencv-python | https://pypi.org/project/opencv-python/ |
| openpyxl | 3.1.5 | MIT | Copyright (c) 2010 openpyxl | https://pypi.org/project/openpyxl/ |
| packaging | 26.3 | Apache-2.0 OR BSD-2-Clause | Copyright (c) 2014-2025 Donald Stufft | https://github.com/pypa/packaging |
| paramiko | 5.1.0 | LGPL-2.1 **[LGPL]** | Copyright (c) 2003-2025 Robey Pointer | https://github.com/paramiko/paramiko |
| pdfminer.six | 20260107 | MIT | https://github.com/pdfminer/pdfminer.six | https://github.com/pdfminer/pdfminer.six |
| pdfplumber | 0.11.10 | (见上游) | Copyright (c) 2019-2025 Jeremy Singer-Vine | https://pypi.org/project/pdfplumber/ |
| pillow | 13.0.0 | MIT-CMU | Copyright (c) 1995-2025 by Fredrik Lundh & Secret Labs AB | https://python-pillow.org |
| pip | 26.2.1 | MIT | Copyright (c) 2008-2025 The pip developers (MIT); zincify portions PSF | https://pip.pypa.io/ |
| protobuf | 7.36.2 | 3-Clause BSD License | Copyright 2008 Google Inc. | https://pypi.org/project/protobuf/ |
| pyclipper | 1.4.0 | MIT | Copyright (c) 2013-2025 Maximilian Körner; Clipper (c) Angus Johnson | https://pypi.org/project/pyclipper/ |
| pycparser | 3.12 | BSD-3-Clause | Copyright (c) 2008-2025 Eli Bendersky | https://github.com/eliben/pycparser |
| PyNaCl | 1.6.2 | Apache-2.0 | : https://github.com/pyca/pynacl/ | https://pypi.org/project/PyNaCl/ |
| pypdf | 6.21.1 | BSD-3-Clause | Copyright (c) 2006-2025 py-pdf contributors | https://github.com/py-pdf/pypdf |
| pypdfium2 | 5.14.0 | BSD-3-Clause, Apache-2.0, dependency licenses | Copyright (c) 2021-2025 pypdfium2-team; PDFium (c) PDFium Authors | https://pypi.org/project/pypdfium2/ |
| python-docx | 1.2.0 | MIT | https://github.com/python-openxml/python-docx | https://github.com/python-openxml/python-docx |
| python-pptx | 1.0.2 | MIT | https://github.com/scanny/python-pptx | https://github.com/scanny/python-pptx |
| pywin32 | 312 | PSF | Copyright (c) 1996-2025 Mark Hammond | https://github.com/mhammond/pywin32 |
| PyYAML | 6.0.3 | MIT | : https://pyyaml.org/ | https://pypi.org/project/PyYAML/ |
| rapidocr-onnxruntime | 1.4.4 | Apache-2.0 | : https://github.com/RapidAI/RapidOCR | https://pypi.org/project/rapidocr-onnxruntime/ |
| requests | 2.34.2 | Apache-2.0 | Copyright (c) 2012-2025 Kenneth Reitz | https://github.com/psf/requests |
| setuptools | 84.2.0 | MIT | Copyright (c) 2016-2025 Python Packaging Authority | https://github.com/pypa/setuptools |
| shapely | 2.2.0 | BSD-3-Clause | Copyright (c) 2007-2025 Sean Gillies; GEOS (c) GEOS contributors | https://github.com/shapely/shapely |
| sherpa-onnx-core | 1.13.8 | Apache-2.0 | : https://github.com/k2-fsa/sherpa-onnx | https://pypi.org/project/sherpa-onnx-core/ |
| sherpa-onnx | 1.13.8 | Apache licensed, as found in the LICENSE file | Copyright (c) 2022-2025 k2-fsa contributors | https://pypi.org/project/sherpa-onnx/ |
| six | 1.17.0 | MIT | Copyright (c) 2010-2024 Benjamin Peterson | https://pypi.org/project/six/ |
| typing_extensions | 4.16.0 | PSF-2.0 | Copyright (c) The Python Software Foundation | https://github.com/python/typing_extensions |
| urllib3 | 2.8.0 | MIT | Copyright (c) 2008-2025 Andrey Petrov and contributors | https://pypi.org/project/urllib3/ |
| websocket-client | 1.9.2 | Apache-2.0 | : https://github.com/websocket-client/websocket-client | https://pypi.org/project/websocket-client/ |
| wheel | 0.48.0 | MIT | Copyright (c) 2012-2025 The wheel developers | https://github.com/pypa/wheel |
| xlsxwriter | 3.2.9 | BSD-2-Clause | Copyright (c) 2013-2025 John McNamara | https://pypi.org/project/xlsxwriter/ |

## 3. 内置组件 wmcppkg（Windows-MCP 运行时，去重后独有 67 包；与 §2.1 同名同版本的包不重复列出）

| 组件 | 版本 | License | 版权 | 来源 |
|---|---|---|---|---|
| aiofile | 3.12.3 | Apache-2.0 | Copyright (c) 2019 Dmitry Orlov | https://github.com/mosquito/aiofile |
| annotated-types | 0.8.0 | MIT | https://github.com/annotated-types/annotated-types | https://github.com/annotated-types/annotated-types |
| attrs | 26.1.0 | MIT | Copyright (c) 2015-2025 Hynek Schlawack | https://pypi.org/project/attrs/ |
| Authlib | 1.9.2 | BSD-3-Clause | https://github.com/authlib/authlib | https://github.com/authlib/authlib |
| backoff | 2.2.1 | MIT | Copyright (c) 2014 litl, LLC. | https://pypi.org/project/backoff/ |
| beartype | 0.23.1 | MIT | Copyright (c) 2021-2025 Beartype authors | https://pypi.org/project/beartype/ |
| beautifulsoup4 | 4.15.0 | MIT License | Copyright (c) 2004-2025 Leonard Richardson | https://www.crummy.com/software/BeautifulSoup/bs4/ |
| cachetools | 7.2.8 | MIT | Copyright (c) 2014-2025 Thomas Kemmer | https://github.com/tkem/cachetools/ |
| caio | 0.12.10 | Apache-2.0 | Copyright (c) 2020 Dmitry Orlov | https://pypi.org/project/caio/ |
| cyclopts | 5.3.3 | Apache-2.0 | Copyright (c) 2023-2025 Brian Pugh | https://github.com/BrianPugh/cyclopts |
| distro | 1.9.0 | Apache License, Version 2.0 | Copyright (c) 2015-2025 Nir Cohen; 2020-2025 distro maintainers | https://pypi.org/project/distro/ |
| dnspython | 2.9.1 | ISC | Copyright (c) 2003-2025 Bob Halley | https://pypi.org/project/dnspython/ |
| docstring_parser | 0.18.0 | MIT | Copyright (c) 2021-2025 Dmitry Shachnev | https://pypi.org/project/docstring_parser/ |
| dxcam | 0.3.0 | MIT | Copyright (c) 2022-2025 ra1nty | https://github.com/ra1nty/DXcam |
| email-validator | 2.3.0 | Unlicense | : https://github.com/JoshData/python-email-validator | https://pypi.org/project/email-validator/ |
| exceptiongroup | 1.3.1 | (见上游) | Copyright (c) 2020-2025 Alex Grönholm | https://pypi.org/project/exceptiongroup/ |
| fastmcp | 4.1.5 | Apache-2.0 | Copyright (c) 2024-2025 Jeremiah Lowin | https://gofastmcp.com |
| fastmcp-slim | 4.1.5 | Apache-2.0 | https://gofastmcp.com | https://gofastmcp.com |
| griffe | 2.4.1 | ISC | Copyright (c) 2021-2025 Timothée Mazzucotelli | https://mkdocstrings.github.io/griffe |
| joserfc | 1.7.7 | BSD-3-Clause | Copyright (c) 2023-2025 Hsiaoming Yang | https://github.com/authlib/joserfc |
| jsonref | 1.1.0 | MIT | Copyright (c) 2021-2025 Chase Sterling | https://pypi.org/project/jsonref/ |
| jsonschema | 4.26.0 | MIT | Copyright (c) 2013-2025 Julian Berman | https://github.com/python-jsonschema/jsonschema |
| jsonschema-path | 0.5.0 | Apache-2.0 | https://github.com/p1c2u/jsonschema-path | https://github.com/p1c2u/jsonschema-path |
| jsonschema-specifications | 2025.9.1 | MIT | https://github.com/python-jsonschema/jsonschema-specifications | https://github.com/python-jsonschema/jsonschema-specifications |
| markdown-it-py | 4.2.0 | (见上游) | https://github.com/executablebooks/markdown-it-py | https://github.com/executablebooks/markdown-it-py |
| markdownify | 1.2.3 | (见上游) | Copyright 2012-2018 Matthew Tretter | http://github.com/matthewwithanm/python-markdownify |
| mcp | 2.3.0 | MIT | Copyright (c) 2024 Model Context Protocol a Series of LF Projects, LLC | https://modelcontextprotocol.io |
| mcp-types | 2.3.0 | MIT | https://modelcontextprotocol.io | https://modelcontextprotocol.io |
| mdurl | 0.1.2 | (见上游) | Copyright (c) 2015 Vitaly Puzrin, Alex Kocharin | https://github.com/executablebooks/mdurl |
| openapi-pydantic | 0.6.0 | MIT | https://github.com/mike-oakley/openapi-pydantic | https://github.com/mike-oakley/openapi-pydantic |
| opentelemetry-api | 1.45.2 | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-python/tree/main/opentelemetry-api | https://github.com/open-telemetry/opentelemetry-python/tree/main/opentelemetry-api |
| pathable | 0.6.0 | Apache-2.0 | Copyright (c) 2021-2025 Artur S. Mąka | https://github.com/p1c2u/pathable |
| platformdirs | 4.14.3 | MIT | Copyright (c) 2010-2025 Jeff Forcier; 2021-2025 platformdirs contributors | https://github.com/tox-dev/platformdirs |
| posthog | 7.82.1 | MIT | Copyright (c) 2020 PostHog Inc. | https://pypi.org/project/posthog/ |
| psutil | 7.2.2 | BSD-3-Clause | Copyright (c) 2009 Jay Loden, Dave Daeschler, Giampaolo Rodola | https://pypi.org/project/psutil/ |
| py-key-value-aio | 0.4.6 | Apache-2.0 |  | https://pypi.org/project/py-key-value-aio/ |
| pydantic | 2.14.2 | MIT | Copyright (c) 2017-2025 Samuel Colvin | https://github.com/pydantic/pydantic |
| pydantic-settings | 2.16.2 | MIT | https://github.com/pydantic/pydantic-settings | https://github.com/pydantic/pydantic-settings |
| pydantic-core | 2.50.2 | MIT | Copyright (c) 2023-2025 Pydantic Services Inc. | https://github.com/pydantic |
| Pygments | 2.21.0 | BSD-2-Clause | https://pygments.org | https://pygments.org |
| PyJWT | 2.15.1 | MIT | https://github.com/jpadilla/pyjwt | https://github.com/jpadilla/pyjwt |
| pyperclip | 1.11.0 | BSD | Copyright (c) 2014-2025 Al Sweigart | https://github.com/asweigart/pyperclip |
| python-dotenv | 1.2.4 | BSD-3-Clause | https://github.com/theskumar/python-dotenv | https://github.com/theskumar/python-dotenv |
| python-multipart | 0.0.33 | Apache-2.0 | https://github.com/Kludex/python-multipart | https://github.com/Kludex/python-multipart |
| RapidFuzz | 3.14.6 | MIT | https://github.com/rapidfuzz/RapidFuzz | https://github.com/rapidfuzz/RapidFuzz |
| referencing | 0.37.0 | MIT | Copyright (c) 2022-2025 Julian Berman | https://github.com/python-jsonschema/referencing |
| rich | 15.0.0 | MIT | Copyright (c) 2020-2025 Will McGugan | https://github.com/Textualize/rich |
| rich-rst | 2.2.0 | MIT | https://github.com/wasi-master/rich-rst | https://github.com/wasi-master/rich-rst |
| rpds-py | 2026.9.1 | MIT | https://github.com/crate-py/rpds | https://github.com/crate-py/rpds |
| soupsieve | 3.0.4 | MIT | Copyright (c) 2012-2025 Isaac Muse | https://github.com/facelessuser/soupsieve |
| sse-starlette | 3.6.0 | BSD-3-Clause | https://github.com/sysid/sse-starlette | https://github.com/sysid/sse-starlette |
| starlette | 1.10.0 | BSD-3-Clause | Copyright (c) 2018-2025 Tom Christie | https://github.com/Kludex/starlette |
| tabulate | 0.10.0 | MIT | Copyright (c) 2011-2025 Asterios Dimitriou | https://github.com/astanin/python-tabulate |
| thefuzz | 0.22.1 | MIT | Copyright (c) 2014 SeatGeek | https://pypi.org/project/thefuzz/ |
| typing-inspection | 0.4.4 | MIT | https://github.com/pydantic/typing-inspection | https://github.com/pydantic/typing-inspection |
| uncalled-for | 0.4.1 | MIT | https://github.com/chrisguidry/uncalled-for | https://github.com/chrisguidry/uncalled-for |
| uuid7 | 0.1.0 | MIT license | Copyright (c) 2021 Stephen Simmons | https://pypi.org/project/uuid7/ |
| uvicorn | 0.54.0 | BSD-3-Clause | Copyright (c) 2017-2025 Encode OSS Ltd | https://uvicorn.dev/ |
| watchfiles | 1.3.0 | MIT | Copyright (c) 2022-2025 Samuel Colvin | https://github.com/samuelcolvin/watchfiles |
| websockets | 17.4 | BSD-3-Clause | Copyright (c) 2013-2025 Aymeric Augustin | https://github.com/python-websockets/websockets |
| windows-mcp | 0.8.8 | MIT License | https://github.com/CursorTouch | https://github.com/CursorTouch |

> 标注 **[LGPL]** / **[MPL]** 的组件有额外义务，见 §8 专项说明。
> 另注：wmcppkg 内的 `fuzzywuzzy` 目录为本产品自有的几行兼容 shim
> （Apache-2.0，把 `import fuzzywuzzy` 转发到 thefuzz，供 windows-mcp 使用），
> 不是 fuzzywuzzy 原包——原包（GPLv2）与 python-Levenshtein（GPL-2.0+）
> 已于 2026-11 从分发清单移除，功能由 thefuzz（MIT）+ RapidFuzz（MIT）覆盖，
> wmcppkg 现不含任何 GPL 组件。

## 4. 随包分发的可执行组件

| 组件 | 版本 | 版权 | License | 来源 | 义务 |
|---|---|---|---|---|---|
| aria2 | 1.37.0 | Copyright (c) 2006, 2019 Tatsuhiro Tsujikawa | **GPL-2.0（含 OpenSSL 链接例外）** | https://github.com/aria2/aria2 （源码 tag release-1.37.0） | 见 §8.1；bin 目录随附 COPYING 与 LICENSE.OpenSSL |
| cua-driver（自研，含第三方派生） | — | Copyright (c) 2025 Cua AI, Inc. 及各方 | MIT（组件内自带 THIRD_PARTY_NOTICES.md，列 trope-cua / Interface-Agent / yabai / QuickJS-ng / rquickjs / T3 Code） | 见 内置组件/cua-driver/THIRD_PARTY_NOTICES.md | 已随组件分发声明 |
| windows-mcp | 0.8.8 | Copyright (c) CursorTouch contributors | MIT | https://github.com/CursorTouch/Windows-MCP | 保留版权行 |
| MD查看器（自研，内嵌组件） | — | 见下列行 | — | — | — |
| ↳ Microsoft.Web.WebView2.Core / WinForms | — | Copyright (c) Microsoft Corporation | MIT | https://github.com/MicrosoftEdge/WebView2SDK | 保留版权行 |
| ↳ WebView2Loader.dll | — | Copyright (c) Microsoft Corporation | 微软 SDK 可再分发条款 | 同上 | 允许随应用分发 |
| ↳ marked | 12.0.0 | Copyright (c) 2018-2024, Christopher Jeffrey | MIT | https://github.com/markedjs/marked | 保留版权行 |
| voicetype（组件 voice 子目录，含 go-webview2 源码副本与 WebView2Loader.dll） | — | 见 §1 go-webview2 行 | MIT | 同 §1 | 保留版权行 |

## 5. 模型文件（组件/voice/models 与 rapidocr 包内嵌）

| 模型 | 来源 | License | 说明 |
|---|---|---|---|
| X-ASR 中文流式识别模型（zipformer transducer，encoder/decoder/joiner/tokens/bpe） | https://github.com/Gilgamesh-J/X-ASR ；模型卡 https://huggingface.co/GilgameshWind/X-ASR-zh-en | Apache-2.0 | 随包分发为上游导出件，未修改 |
| silero VAD（silero_vad.onnx） | https://github.com/snakers4/silero-vad | MIT，Copyright (c) 2020-present Silero Team | 未修改 |
| PP-OCRv4 det/cls/rec（rapidocr_onnxruntime 内嵌） | https://github.com/RapidAI/RapidOCR （模型出自 PaddleOCR） | Apache-2.0 | RapidOCR 官方发行件，未修改 |

## 6. 前端（web/，go:embed 内嵌）

| 组件 | 版本 | 版权 | License | 来源 |
|---|---|---|---|---|
| qrcode（npm 包，浏览器版打包） | 1.5.4 | Copyright (c) 2012 Ryan Day | MIT | https://github.com/soldair/node-qrcode |

## 7. 安卓（jx-link 壳 / android 主壳 / Termux 运行时快照）

| 组件 | 版本 | 版权 | License | 来源 |
|---|---|---|---|---|
| androidx.core:core-ktx | 1.15.0 | Copyright (c) The Android Open Source Project | Apache-2.0 | https://cs.android.com/androidx/platform/frameworks/support |
| androidx.activity | 1.9.3 | 同上 | Apache-2.0 | 同上 |
| androidx.webkit | 1.12.1 | 同上 | Apache-2.0 | 同上 |
| androidx.camera（core/camera2/lifecycle/view） | 1.3.4 | 同上 | Apache-2.0 | 同上 |
| ZXing core | 3.5.3 | Copyright (c) 2007-2025 ZXing authors | Apache-2.0 | https://github.com/zxing/zxing |
| Kotlin 标准库（编译期） | — | Copyright (c) JetBrains | Apache-2.0 | https://github.com/JetBrains/kotlin |
| Gradle Wrapper（仓库内） | — | Copyright (c) Gradle Inc. | Apache-2.0 | https://gradle.org |
| **Termux 运行时快照（runtime.tgz：bootstrap + apt 包闭包 + Python 3.14 + openssh/curl/proot 等）** | 见 android/runtime/MANIFEST.json | 各包版权人各自所有 | **逐包各自协议（GPL-3+/GPL-2+/PSF/BSD/curl/ISC 等）** | 镜像源（源码同路径 src/ 可取）：http://mirrors.ustc.edu.cn/termux/apt/termux-main | 见 §8.2 |

## 8. copyleft / 特殊义务专项

### 8.1 GPL-2.0（aria2 1.37.0）

aria2 以独立进程方式调用，与本产品其余部分进程隔离，许可互不影响。
对应源码：https://github.com/aria2/aria2/tree/release-1.37.0 （亦见官方
https://github.com/aria2/aria2/releases/tag/release-1.37.0 ）。GPLv2 全文
随安装包分发（技能/downloader/bin/COPYING），OpenSSL 链接例外声明见同目录
LICENSE.OpenSSL。本产品分发 aria2 的官方发行二进制，未作修改。

### 8.2 Termux 运行时快照（GPL 密集，安卓侧）

runtime.tgz 含 GNU coreutils/bash（GPL-3+）、apt/dpkg（GPL-2+）、proot
（GPL-2+）等 GPL 二进制及 Python（PSF）、openssh（BSD/ISC）、curl（curl
license）等。逐包清单与版本见 android/runtime/MANIFEST.json；各包源码可从
构建镜像源获取（apt source 同源，USTC 镜像长期可用）。GPLv2/GPLv3 全文见
https://www.gnu.org/licenses/old-licenses/gpl-2.0.html 与
https://www.gnu.org/licenses/gpl-3.0.html 。

### 8.3 LGPL-2.1（paramiko 5.1.0）

作为独立库动态加载（Python import），未修改；使用方（本产品）允许用户
逆向调试与替换该库。库源码：https://github.com/paramiko/paramiko （tag 对应
版本）。LGPL-2.1 全文：https://www.gnu.org/licenses/old-licenses/lgpl-2.1.html

### 8.4 MPL-2.0（certifi）

原样分发未修改，MPL 文件级条款随 wheel 内许可头满足；源码：
https://github.com/certifi/python-certifi 。
MPL-2.0 全文：https://mozilla.org/MPL/2.0/

---

本文件按本仓库当前分发清单维护；依赖更新时请同步更新本声明。
