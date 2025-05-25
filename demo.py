#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Pan123 API 重构后的演示程序
测试各种功能模块
"""

import sys
import traceback
from api import Pan123Client, Pan123APIError, AuthenticationError, ConfigurationError


def print_separator(title: str):
    """打印分隔符"""
    print(f"\n{'='*50}")
    print(f" {title}")
    print(f"{'='*50}")


def test_client_initialization():
    """测试客户端初始化"""
    print_separator("测试客户端初始化")

    try:
        # 测试使用默认配置初始化
        client = Pan123Client(enable_cache=True, redis_host="192.168.2.254")
        print("✓ 客户端初始化成功")
        print(f"✓ 认证状态: {'已认证' if client.is_authenticated() else '未认证'}")

        # 获取缓存统计
        cache_stats = client.get_cache_stats()
        print(f"✓ 缓存状态: {cache_stats}")

        return client

    except ConfigurationError as e:
        print(f"✗ 配置错误: {e}")
        print("请确保 config.json 文件存在且配置正确")
        return None
    except AuthenticationError as e:
        print(f"✗ 认证错误: {e}")
        return None
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()
        return None


def test_list_files(client: Pan123Client):
    """测试文件列表功能"""
    print_separator("测试文件列表功能")

    try:
        # 列出根目录的文件
        print("📁 列出根目录文件...")
        file_list, next_file_id = client.list_files(parent_id=0, limit=10)

        print(f"✓ 获取到 {len(file_list)} 个文件/文件夹")

        if next_file_id:
            print(f"✓ 下一页标识: {next_file_id}")

        # 显示文件信息
        for i, file_obj in enumerate(file_list):
            file_type = "📁" if file_obj.is_folder else "📄"
            print(f"  {i+1}. {file_type} {file_obj.filename}")
            print(
                f"     ID: {file_obj.file_id} | 大小: {file_obj.size_formatted}")
            print(f"     分类: {file_obj.category_name} | 图标: {file_obj.icon}")

        return file_list

    except Pan123APIError as e:
        print(f"✗ API错误: {e}")
        return None
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()
        return None


def test_search_files(client: Pan123Client):
    """测试文件搜索功能"""
    print_separator("测试文件搜索功能")

    try:
        # 搜索mp4文件
        search_keyword = "mp4"
        print(f"🔍 搜索关键词: '{search_keyword}'...")

        file_list, next_file_id = client.list_files(
            search_data=search_keyword,
            search_mode=0,  # 模糊搜索
            limit=5
        )

        print(f"✓ 搜索到 {len(file_list)} 个文件")

        # 显示搜索结果
        for i, file_obj in enumerate(file_list):
            file_type = "📁" if file_obj.is_folder else "📄"
            print(f"  {i+1}. {file_type} {file_obj.filename}")
            print(
                f"     ID: {file_obj.file_id} | 大小: {file_obj.size_formatted}")
            print(f"     扩展名: {file_obj.file_extension}")

        return file_list

    except Pan123APIError as e:
        print(f"✗ 搜索失败: {e}")
        return None
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()
        return None


def test_pagination(client: Pan123Client):
    """测试分页功能"""
    print_separator("测试分页功能")

    try:
        import time

        print("📄 测试手动分页...")
        # 测试手动分页
        page1_files, next_id = client.list_files(parent_id=0, limit=1)
        print(f"✓ 第1页: 获取到 {len(page1_files)} 个文件")

        if next_id:
            page2_files, next_id2 = client.list_files(
                parent_id=0, limit=100, last_file_id=next_id)
            print(f"✓ 第2页: 获取到 {len(page2_files)} 个文件")
            print(f"✓ 下一页ID: {next_id2}")

        print("\n🔄 测试自动分页（QPS限制: 1.5 req/s）...")
        start_time = time.time()

        # 测试自动获取所有分页
        all_files, _ = client.list_files(
            parent_id=0,
            limit=8,  # 每页8个文件，便于测试分页
            auto_fetch_all=True,
            qps_limit=1.5  # 每秒1.5个请求
        )

        end_time = time.time()
        elapsed = end_time - start_time

        print(f"✓ 自动分页完成:")
        print(f"  - 总文件数: {len(all_files)}")
        print(f"  - 总耗时: {elapsed:.2f} 秒")
        if elapsed > 0:
            print(f"  - 实际QPS: {len(all_files) / elapsed:.2f}")

        # 显示前几个文件
        print(f"\n📋 前5个文件:")
        for i, file_obj in enumerate(all_files.files[:5]):
            file_type = "📁" if file_obj.is_folder else "📄"
            print(
                f"  {i+1}. {file_type} {file_obj.filename} ({file_obj.size_formatted})")

        return all_files

    except Pan123APIError as e:
        print(f"✗ 分页测试失败: {e}")
        return None
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()
        return None


def test_file_details(client: Pan123Client, file_list):
    """测试文件详情获取"""
    print_separator("测试文件详情获取")

    if not file_list or len(file_list) == 0:
        print("⚠️ 没有可用的文件ID进行测试")
        return

    try:
        # 收集一些文件ID
        file_ids = [f.file_id for f in file_list[:3] if f.file_id]

        if not file_ids:
            print("⚠️ 没有有效的文件ID")
            return

        print(f"📋 获取文件详情，ID列表: {file_ids}")

        # 测试批量获取（使用缓存）
        print("\n🔄 测试缓存功能...")
        detailed_files = client.get_files_info(file_ids, use_cache=True)

        print(f"✓ 获取到 {len(detailed_files)} 个文件的详情")

        for file_obj in detailed_files:
            print(f"\n📄 文件: {file_obj.filename}")
            print(f"   ID: {file_obj.file_id}")
            print(f"   大小: {file_obj.size_formatted}")
            print(f"   类型: {'文件夹' if file_obj.is_folder else '文件'}")
            print(f"   分类: {file_obj.category_name}")
            print(f"   创建时间: {file_obj.create_at}")
            print(f"   更新时间: {file_obj.update_at}")
            print(f"   MD5: {file_obj.etag}")
            print(f"   父目录ID: {file_obj.parent_file_id}")

        # 测试单个文件获取
        print(f"\n🔍 测试单个文件获取...")
        single_file = client.get_file_info_single(file_ids[0])
        if single_file:
            print(f"✓ 单个文件获取成功: {single_file.filename}")

        return detailed_files

    except Pan123APIError as e:
        print(f"✗ 获取文件详情失败: {e}")
        return None
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()
        return None


def test_download_info(client: Pan123Client, file_list):
    """测试下载链接获取"""
    print_separator("测试下载链接获取")

    if not file_list:
        print("⚠️ 没有可用的文件进行测试")
        return

    # 找一个非文件夹的文件
    target_file = None
    for file_obj in file_list:
        if not file_obj.is_folder:
            target_file = file_obj
            break

    if not target_file:
        print("⚠️ 没有找到可下载的文件（非文件夹）")
        return

    try:
        print(f"📥 获取文件下载链接: {target_file.filename}")
        print(f"   文件ID: {target_file.file_id}")

        download_info = client.get_download_info(target_file.file_id)

        if download_info and 'data' in download_info:
            download_data = download_info['data']
            download_url = download_data.get('downloadUrl')

            if download_url:
                print(f"✓ 下载链接获取成功")
                print(f"   下载URL: {download_url[:100]}...")

                # 显示其他下载信息
                if 'filename' in download_data:
                    print(f"   文件名: {download_data['filename']}")
                if 'size' in download_data:
                    print(f"   文件大小: {download_data['size']} 字节")
            else:
                print("✗ 响应中没有下载链接")
        else:
            print("✗ 无效的下载信息响应")

    except Pan123APIError as e:
        print(f"✗ 获取下载链接失败: {e}")
    except Exception as e:
        print(f"✗ 未知错误: {e}")
        traceback.print_exc()


def test_cache_operations(client: Pan123Client):
    """测试缓存操作"""
    print_separator("测试缓存操作")

    try:
        # 获取缓存统计
        stats = client.get_cache_stats()
        print(f"📊 缓存统计: {stats}")

        if stats.get('enabled'):
            print("🗑️ 测试清除所有缓存...")
            client.clear_file_cache()
            print("✓ 缓存清除完成")

            # 再次获取统计
            new_stats = client.get_cache_stats()
            print(f"📊 清除后统计: {new_stats}")
        else:
            print("⚠️ 缓存未启用")

    except Exception as e:
        print(f"✗ 缓存操作失败: {e}")
        traceback.print_exc()


def test_error_handling(client: Pan123Client):
    """测试错误处理"""
    print_separator("测试错误处理")

    try:
        # 测试无效文件ID
        print("🧪 测试无效文件ID...")
        try:
            invalid_file = client.get_file_info_single(999999999)
            if invalid_file is None:
                print("✓ 无效文件ID正确返回None")
            else:
                print(f"⚠️ 意外获取到文件: {invalid_file}")
        except Pan123APIError as e:
            print(f"✓ 正确捕获API错误: {e}")

        # 测试无效参数
        print("\n🧪 测试无效参数...")
        try:
            client.get_files_info(["invalid"])  # 传入字符串而不是整数
        except Exception as e:
            print(f"✓ 正确捕获参数错误: {type(e).__name__}: {e}")

    except Exception as e:
        print(f"✗ 错误处理测试失败: {e}")
        traceback.print_exc()


def test_file_path(client: Pan123Client):
    """测试文件路径获取功能"""
    print_separator("测试文件路径获取功能")

    try:
        # 先获取一些文件
        print("📁 获取测试文件...")
        file_list, _ = client.list_files(parent_id=0, limit=10)

        if not file_list or len(file_list) == 0:
            print("⚠️ 没有找到测试文件")
            return

        # 测试文件路径获取
        test_files = file_list.files[:3]  # 取前3个文件进行测试

        for i, file_obj in enumerate(test_files):
            print(f"\n📄 测试文件 {i+1}: {file_obj.filename}")
            print(f"   文件ID: {file_obj.file_id}")
            print(f"   父目录ID: {file_obj.parent_file_id}")

            # 获取简单路径
            print("   🔍 获取文件路径...")
            file_path = client.get_file_path(file_obj.file_id)
            if file_path:
                print(f"   ✓ 完整路径: {file_path}")
            else:
                print("   ✗ 获取路径失败")

            # 获取详细路径信息
            print("   🔍 获取详细路径信息...")
            path_details = client.get_file_path_with_details(file_obj.file_id)
            if path_details:
                print(f"   ✓ 详细路径: {path_details['full_path']}")
                print(f"   ✓ 路径深度: {path_details['depth']}")
                print(f"   ✓ 路径组件数: {len(path_details['path_components'])}")

                # 显示路径组件
                if path_details['path_components']:
                    print("   📂 路径组件:")
                    for j, component in enumerate(path_details['path_components']):
                        indent = "   " + "  " * (j + 1)
                        comp_type = "📁" if component['is_folder'] else "📄"
                        print(
                            f"{indent}{comp_type} {component['name']} (ID: {component['file_id']})")
            else:
                print("   ✗ 获取详细路径失败")

        # 测试根目录文件的路径
        print(f"\n🏠 测试根目录文件路径...")
        if test_files:
            root_file = test_files[0]
            if root_file.parent_file_id == 0:
                print(f"   文件 '{root_file.filename}' 在根目录")
                path = client.get_file_path(root_file.file_id)
                print(f"   根目录文件路径: {path}")

    except Exception as e:
        print(f"✗ 文件路径测试失败: {e}")
        import traceback
        traceback.print_exc()


def main():
    """主函数"""
    print("🚀 Pan123 API 重构版本功能测试")
    print("=" * 60)

    # 1. 测试客户端初始化
    client = test_client_initialization()
    if not client:
        print("\n❌ 客户端初始化失败，无法继续测试")
        sys.exit(1)

    try:
        # 使用上下文管理器
        with client:
            # 2. 测试文件列表
            file_list = test_list_files(client)

            # 3. 测试文件搜索
            search_results = test_search_files(client)

            # 4. 测试分页功能
            paginated_files = test_pagination(client)

            # 5. 测试文件详情（使用列表结果）
            test_files = file_list if file_list else search_results
            detailed_files = test_file_details(client, test_files)

            # 6. 测试下载链接
            test_download_info(client, test_files)

            # 7. 测试文件路径功能
            test_file_path(client)

            # 8. 测试缓存操作
            test_cache_operations(client)

            # 9. 测试错误处理
            test_error_handling(client)

    except KeyboardInterrupt:
        print("\n\n⚠️ 用户中断测试")
    except Exception as e:
        print(f"\n\n❌ 测试过程中发生错误: {e}")
        traceback.print_exc()

    print_separator("测试完成")
    print("✨ 重构后的API测试已完成！")


if __name__ == "__main__":
    main()
