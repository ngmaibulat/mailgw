"use strict";
module.exports = {
    async up(queryInterface, Sequelize) {
        await queryInterface.createTable("Logs", {
            id: {
                allowNull: false,
                autoIncrement: true,
                primaryKey: true,
                type: Sequelize.INTEGER,
            },
            url: {
                type: Sequelize.STRING(2048),
            },
            path: {
                type: Sequelize.STRING,
            },
            query: {
                type: Sequelize.STRING,
            },
            src_ip: {
                type: Sequelize.STRING,
            },
            src_port: {
                type: Sequelize.INTEGER,
            },
            referer: {
                type: Sequelize.STRING,
            },
            origin: {
                type: Sequelize.STRING,
            },
            method: {
                type: Sequelize.STRING,
            },
            user: {
                type: Sequelize.STRING,
            },
            userAgent: {
                type: Sequelize.STRING,
            },
            createdAt: {
                allowNull: false,
                type: Sequelize.DATE,
            },
            updatedAt: {
                allowNull: false,
                type: Sequelize.DATE,
            },
        });
    },
    async down(queryInterface, Sequelize) {
        await queryInterface.dropTable("Logs");
    },
};
