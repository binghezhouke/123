import base64

# 定义标准的 Base62 字符集 (0-9, a-z, A-Z)
BASE62_ALPHABET = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"


def base62_decode_to_bytes(b62_string: str) -> bytes:
    """
    将一个 Base62 编码的字符串解码为原始的字节数据 (bytes)。

    Args:
        b62_string: Base62 编码的字符串。

    Returns:
        解码后的字节数据。

    Raises:
        ValueError: 如果输入字符串包含 Base62 字符集之外的字符。
    """
    base = len(BASE62_ALPHABET)
    num = 0

    # 将 Base62 字符串转换为一个大整数
    for char in b62_string:
        try:
            position = BASE62_ALPHABET.index(char)
            num = num * base + position
        except ValueError:
            raise ValueError(f"无效的 Base62 字符: '{char}'")

    # 如果整数为0，则字节长度为1 (代表b'\x00')
    if num == 0:
        return b'\x00'

    # 将大整数转换为字节。
    # (num.bit_length() + 7) // 8 用于计算存储该整数所需的最少字节数。
    byte_length = (num.bit_length() + 7) // 8
    return num.to_bytes(byte_length, 'big')


def base62_to_hex(b62_string: str) -> str:
    """
    将一个 Base62 编码的字符串转换为 Base64 编码的字符串。

    Args:
        b62_string: Base62 编码的字符串。

    Returns:
        转换后的 Base64 编码字符串。
    """
    # 步骤 1: 将 Base62 字符串解码为原始字节
    try:
        decoded_bytes = base62_decode_to_bytes(b62_string)
    except ValueError as e:
        # 如果解码失败，返回错误信息
        return f"转换失败: {e}"

    return decoded_bytes.hex()
