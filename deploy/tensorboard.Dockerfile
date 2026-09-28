# TensorBoard for watching training runs live. Reads every run's tb/ directory under
# /data/models (mounted read-only at /logs).
FROM python:3.12-slim
RUN pip install --no-cache-dir "tensorboard>=2.20,<3"
EXPOSE 6006
ENTRYPOINT ["tensorboard", "--logdir", "/logs", "--host", "0.0.0.0", "--port", "6006", "--reload_interval", "15"]
