#!/bin/bash
export LD_LIBRARY_PATH=/home/kevyn/llama-cpp-turboquant-fresh/build-fresh/bin${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
exec /home/kevyn/llama-cpp-turboquant-fresh/build-fresh/bin/llama-server "$@"
