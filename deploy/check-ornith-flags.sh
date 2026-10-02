#!/bin/bash
/opt/ai/bin/llama-server --help 2>/dev/null | grep -E "model-draft|spec-draft|spec-type|n-cpu-moe|jinja"
