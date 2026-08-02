'use strict';
module.exports = {
  async up(queryInterface, Sequelize) {
    await queryInterface.createTable('Delivery', {
      
      id: {
        allowNull: false,
        autoIncrement: true,
        primaryKey: true,
        type: Sequelize.INTEGER
      },

      dt: {
        type: Sequelize.DATE
      },

      uuid: {
        type: Sequelize.STRING
      },

      sender: {
        type: Sequelize.STRING
      },

      rcpt_list: {
        type: Sequelize.STRING
      },

      rcpt_domain: {
        type: Sequelize.STRING
      },

      rcpt_accepted: {
        type: Sequelize.STRING
      },

      tls_forced: {
        type: Sequelize.INTEGER
      },

      tls: {
        type: Sequelize.INTEGER
      },

      auth: {
        type: Sequelize.INTEGER
      },

      host: {
        type: Sequelize.STRING
      },

      ip: {
        type: Sequelize.STRING
      },

      port: {
        type: Sequelize.INTEGER
      },

      response: {
        type: Sequelize.STRING
      },

      delay: {
        type: Sequelize.DOUBLE
      },

      createdAt: {
        allowNull: false,
        type: Sequelize.DATE
      },

      updatedAt: {
        allowNull: false,
        type: Sequelize.DATE
      }

    });
  },
  async down(queryInterface, Sequelize) {
    await queryInterface.dropTable('Delivery');
  }
};