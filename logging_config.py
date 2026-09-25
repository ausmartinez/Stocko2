import logging
from logging.handlers import RotatingFileHandler
from pathlib import Path

def setup_logger(
    name: str = __name__,
    log_file: str = "app.log",
    level: int = logging.INFO,
) -> logging.Logger:
    """Configures and returns a logger with console and rotating file handlers."""
    # Ensure log directory exists
    log_path = Path("logs")
    log_path.mkdir(exist_ok=True)
    file_destination = log_path / log_file

    logger = logging.getLogger(name)
    logger.setLevel(level)

    # Prevent adding duplicate handlers if logger is initialized multiple times
    if logger.hasHandlers():
        return logger

    # Shared Formatter (Timestamp | Level | Module | Line Number | Message)
    formatter = logging.Formatter(
        "%(asctime)s | %(levelname)-8s | %(module)s:%(lineno)d | %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    # 1. Console Handler (Outputs to terminal)
    console_handler = logging.StreamHandler()
    console_handler.setFormatter(formatter)
    console_handler.setLevel(level)
    logger.addHandler(console_handler)

    # 2. Rotating File Handler (Max 5MB per file, keeps 3 backups)
    file_handler = RotatingFileHandler(
        file_destination, maxBytes=5 * 1024 * 1024, backupCount=3, encoding="utf-8"
    )
    file_handler.setFormatter(formatter)
    file_handler.setLevel(level)
    logger.addHandler(file_handler)

    return logger
