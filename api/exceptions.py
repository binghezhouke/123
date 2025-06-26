"""
自定义异常类
"""


class Pan123APIError(Exception):
    """自定义API错误异常"""

    def __init__(self, message, status_code=None, error_code=None):
        super().__init__(message)
        self.status_code = status_code
        self.error_code = error_code

    def __str__(self):
        error_parts = [str(self.args[0]) if self.args else "Pan123API错误"]

        if self.status_code:
            error_parts.append(f"状态码: {self.status_code}")

        if self.error_code:
            error_parts.append(f"错误码: {self.error_code}")

        return " | ".join(error_parts)


class AuthenticationError(Pan123APIError):
    """认证错误"""
    pass


class ConfigurationError(Pan123APIError):
    """配置错误"""
    pass


class NetworkError(Pan123APIError):
    """网络错误"""
    pass


class ValidationError(Pan123APIError):
    """参数验证错误"""
    pass


class FileUploadError(Pan123APIError):
    """文件上传错误"""
    pass
