import requests
import json
import time


class VKParser:
    def __init__(self, login, password):
        self.login = login
        self.password = password
        self.session = requests.Session()
        self.access_token = None
        self.user_id = None

    def login_vk(self):
        """Авторизация в VK"""
        print("Выполняю авторизацию...")

        # Получить данные для авторизации
        auth_url = "https://oauth.vk.com/authorize"
        auth_params = {
            'client_id': '6121396',
            'redirect_uri': 'https://oauth.vk.com/blank.html',
            'display': 'page',
            'scope': 'wall,offline,groups',  # Добавили права для удаления
            'response_type': 'token',
            'v': '5.131'
        }

        print("Для работы скрипта необходим access_token")
        print("Получите его по ссылке:")
        print(f"{auth_url}?{'&'.join([f'{k}={v}' for k, v in auth_params.items()])}")
        print("\nПосле авторизации скопируйте access_token из адресной строки")

        self.access_token = input("Введите access_token: ").strip()

        if self.access_token:
            user_info = self._make_api_request('users.get', {})
            if user_info and 'response' in user_info:
                self.user_id = user_info['response'][0]['id']
                print(f"Успешная авторизация! User ID: {self.user_id}")
                return True
            else:
                print("Ошибка авторизации. Проверьте токен.")
                return False
        return False

    def _make_api_request(self, method, params):
        """Выполнение запроса к VK API"""
        params['access_token'] = self.access_token
        params['v'] = '5.131'

        try:
            response = self.session.get(
                f'https://api.vk.com/method/{method}',
                params=params,
                timeout=30
            )
            return response.json()
        except Exception as e:
            print(f"Ошибка при запросе к API: {e}")
            return None

    def get_all_posts(self):
        """Получение всех постов со стены"""
        if not self.access_token:
            print("Сначала выполните авторизацию!")
            return []

        print("Начинаю сбор постов...")
        all_posts = []
        offset = 0
        count = 100

        while True:
            print(f"Загружаю посты, offset: {offset}")

            response = self._make_api_request('wall.get', {
                'owner_id': self.user_id,
                'count': count,
                'offset': offset
            })

            if not response or 'response' not in response:
                print("Ошибка при получении постов")
                break

            posts = response['response']['items']
            if not posts:
                break

            all_posts.extend(posts)
            offset += count

            time.sleep(0.5)

            if len(posts) < count:
                break

        return all_posts

    def delete_all_posts(self):
        """Удаление всех постов со стены"""
        if not self.access_token:
            print("Сначала выполните авторизацию!")
            return False

        print("⚠️  ВНИМАНИЕ! ЭТА ОПЕРАЦИЯ НЕОБРАТИМА! ⚠️")
        print("Все посты будут удалены без возможности восстановления!")

        confirmation = input("Вы уверены? (введите 'DELETE ALL' для подтверждения): ")
        if confirmation != "DELETE ALL":
            print("Операция отменена.")
            return False

        posts = self.get_all_posts()
        if not posts:
            print("Постов для удаления нет.")
            return True

        print(f"Найдено постов для удаления: {len(posts)}")

        deleted_count = 0
        error_count = 0

        for i, post in enumerate(posts, 1):
            print(f"Удаляю пост {i}/{len(posts)}...")

            # Что бы удалить пост нужен его ID и owner_id
            response = self._make_api_request('wall.delete', {
                'owner_id': self.user_id,
                'post_id': post['id']
            })

            if response and 'response' in response and response['response'] == 1:
                deleted_count += 1
                print(f"✓ Пост {post['id']} удален")
            else:
                error_count += 1
                print(f"✗ Ошибка удаления поста {post['id']}")
                if 'error' in response:
                    print(f"   Код ошибки: {response['error']['error_code']}")
                    print(f"   Сообщение: {response['error']['error_msg']}")

            # Задержка что б ВК не блочил
            time.sleep(1)

        print(f"\nРезультат удаления:")
        print(f"Успешно удалено: {deleted_count}")
        print(f"Ошибок: {error_count}")

        return error_count == 0

    def delete_posts_with_confirmation(self):
        """Удаление постов с подробным подтверждением"""
        if not self.access_token:
            print("Сначала выполните авторизацию!")
            return False

        posts = self.get_all_posts()
        if not posts:
            print("Постов для удаления нет.")
            return True

        print(f"\nНайдено постов: {len(posts)}")
        print("Первые 5 постов для примера:")

        # Показываем примеры постов
        for i, post in enumerate(posts[:5], 1):
            post_date = time.strftime('%Y-%m-%d', time.localtime(post['date']))
            text_preview = post['text'][:100] + "..." if len(post['text']) > 100 else post['text']
            print(f"{i}. [{post_date}] {text_preview}")

        print(f"\n⚠️  ВНИМАНИЕ! БУДУТ УДАЛЕНЫ ВСЕ {len(posts)} ПОСТОВ! ⚠️")
        print("Это действие нельзя отменить!")

        # Двойное подтверждение
        confirmation1 = input("\nВведите 'DELETE MY POSTS' для подтверждения: ")
        if confirmation1 != "DELETE MY POSTS":
            print("Операция отменена.")
            return False

        confirmation2 = input("Введите 'YES I AM SURE' для окончательного подтверждения: ")
        if confirmation2 != "YES I AM SURE":
            print("Операция отменена.")
            return False

        return self._delete_posts(posts)

    def _delete_posts(self, posts):
        """Внутренняя функция удаления постов"""
        deleted_count = 0
        error_count = 0

        for i, post in enumerate(posts, 1):
            print(f"Удаляю пост {i}/{len(posts)} (ID: {post['id']})...")

            response = self._make_api_request('wall.delete', {
                'owner_id': self.user_id,
                'post_id': post['id']
            })

            if response and 'response' in response and response['response'] == 1:
                deleted_count += 1
            else:
                error_count += 1
                print(f"   Ошибка при удалении поста {post['id']}")

            time.sleep(1)

        print(f"\nУдаление завершено:")
        print(f"Успешно: {deleted_count}")
        print(f"Ошибок: {error_count}")

        return error_count == 0

    def extract_text_from_posts(self, posts):
        """Извлечение текста из постов"""
        texts = []

        for post in posts:
            if 'text' in post and post['text'].strip():
                post_date = time.strftime('%Y-%m-%d %H:%M:%S',
                                          time.localtime(post['date']))
                texts.append(f"Дата: {post_date}\nТекст: {post['text']}\n{'-' * 50}")

        return texts

    def save_posts_to_file(self, texts, filename='vk_posts.txt'):
        """Сохранение постов в файл"""
        with open(filename, 'w', encoding='utf-8') as f:
            f.write(f"Всего постов: {len(texts)}\n")
            f.write("=" * 60 + "\n\n")
            for text in texts:
                f.write(text + "\n\n")

        print(f"Посты сохранены в файл: {filename}")


def main():
    print("VK Parser - Управление постами")
    print("=" * 40)

    login = input("Введите логин VK: ").strip()
    password = input("Введите пароль VK: ").strip()

    parser = VKParser(login, password)

    if parser.login_vk():
        while True:
            print("\nВыберите действие:")
            print("1 - Получить все посты и сохранить в файл")
            print("2 - Просмотреть количество постов")
            print("3 - Удалить все посты (БЫСТРОЕ ПОДТВЕРЖДЕНИЕ)")
            print("4 - Удалить все посты (ПОДРОБНОЕ ПОДТВЕРЖДЕНИЕ)")
            print("0 - Выход")

            choice = input("Ваш выбор: ").strip()

            if choice == "1":
                posts = parser.get_all_posts()
                if posts:
                    post_texts = parser.extract_text_from_posts(posts)
                    parser.save_posts_to_file(post_texts)
                    print(f"Сохранено {len(post_texts)} постов")
                else:
                    print("Постов нет или ошибка получения")

            elif choice == "2":
                posts = parser.get_all_posts()
                print(f"Всего постов на стене: {len(posts)}")

            elif choice == "3":
                if parser.delete_all_posts():
                    print("Удаление завершено успешно")
                else:
                    print("Удаление завершено с ошибками")

            elif choice == "4":
                if parser.delete_posts_with_confirmation():
                    print("Удаление завершено успешно")
                else:
                    print("Удаление завершено с ошибками")

            elif choice == "0":
                print("Выход...")
                break

            else:
                print("Неверный выбор")


if __name__ == "__main__":
    main()