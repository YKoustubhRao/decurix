import time


# ============================================================
# Utility
# ============================================================

def timestamp():
    return time.strftime("%Y-%m-%d %H:%M:%S")


def log(message):
    print(
        f"[{timestamp()}] {message}",
        flush=True
    )


def safe_filename(name):
    return "".join(
        c if c.isalnum() or c in "-_." else "_"
        for c in name
    )
