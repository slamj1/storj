// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

import { Notification, NotificationsApi, NotificationsCursor, NotificationsState } from '@/app/types/notifications';
import { HttpClient } from '@/storagenode/utils/httpClient';

/**
 * NotificationsHttpApi is a http implementation of Notifications API.
 * Exposes all notifications-related functionality
 */
export class NotificationsHttpApi implements NotificationsApi {
    private readonly client: HttpClient = new HttpClient();
    private readonly ROOT_PATH: string = '/api/notifications';

    /**
     * Fetch notifications.
     *
     * @returns notifications state
     * @throws Error
     */
    public async get(cursor: NotificationsCursor): Promise<NotificationsState> {
        const path = `${this.ROOT_PATH}/list?page=${cursor.page}&limit=${cursor.limit}`;
        const response = await this.client.get(path);

        if (!response.ok) {
            throw new Error('can not get notifications');
        }

        const notificationResponse = await response.json();
        let notifications: Notification[] = [];
        let pageCount: number = 0;
        let unreadCount: number = 0;

        if (notificationResponse) {
            notifications = notificationResponse.page.notifications.map(item =>
                new Notification(
                    item.id,
                    item.senderId,
                    item.type,
                    item.title,
                    item.message,
                    !!item.readAt,
                    new Date(item.createdAt),
                ),
            );

            pageCount = notificationResponse.page.pageCount;
            unreadCount = notificationResponse.unreadCount;
        }

        return new NotificationsState(notifications, pageCount, unreadCount);
    }

    /**
     * Marks single notification as read.
     * @throws Error
     */
    public async read(id: string): Promise<void> {
        const path = `${this.ROOT_PATH}/${id}/read`;
        const response = await this.client.post(path, null);

        if (response.ok) {
            return;
        }

        throw new Error('can not mark notification as read');
    }

    /**
     * Marks all notifications as read.
     * @throws Error
     */
    public async readAll(): Promise<void> {
        const path = `${this.ROOT_PATH}/readall`;
        const response = await this.client.post(path, null);

        if (response.ok) {
            return;
        }

        throw new Error('can not mark all notifications as read');
    }
}
