# AkShare sidecar 容器镜像。
#
# 单独一个服务而不是塞进 artex 主镜像：akshare 要拖 pandas/numpy/lxml 一堆
# 依赖，打进主镜像会让每次升级 artex 都重装一遍，且主镜像从 node:slim 起，
# 装 python 栈要另加几百 MB。分开后主镜像保持精简，sidecar 变了才重建它。
#
# 基础镜像与主 Dockerfile 保持一致（README「部署说明」允许的唯一外部镜像）。
FROM python:3.12-slim-bookworm

WORKDIR /app
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt
COPY server.py /app/server.py

# 容器内必须监听 0.0.0.0：默认 127.0.0.1 只有本机可达，别的容器连不进来。
ENV AKSHARE_SIDECAR_ADDR=0.0.0.0:8791
EXPOSE 8791

# 预热要 20s 起，这段窗口端口通但 warmup_done=false —— 健康检查按真实可用性判。
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD python -c "import json,urllib.request,sys; d=json.load(urllib.request.urlopen('http://127.0.0.1:8791/health')); sys.exit(0 if d.get('data',{}).get('warmup_done') else 1)"

CMD ["python", "/app/server.py"]
